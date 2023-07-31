package datamove

import (
	"context"
	"fmt"
	"time"

	"cloud.google.com/go/storage"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/molt/cmd/internal/cmdutil"
	"github.com/cockroachdb/molt/datamove"
	"github.com/cockroachdb/molt/datamove/datamovestore"
	"github.com/cockroachdb/molt/dbconn"
	"github.com/cockroachdb/molt/verify/dbverify"
	"github.com/cockroachdb/molt/verify/tableverify"
	"github.com/spf13/cobra"
	"golang.org/x/oauth2/google"
)

func Command() *cobra.Command {
	var (
		s3Bucket       string
		gcpBucket      string
		localPath      string
		directCRDBCopy bool
		cleanup        bool
		live           bool
		flushSize      int
	)
	cmd := &cobra.Command{
		Use:  "datamove",
		Long: `Moves data from a source to a target.`,

		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()

			conns, err := cmdutil.LoadDBConns(ctx)
			if err != nil {
				return err
			}
			logger, err := cmdutil.Logger()
			if err != nil {
				return err
			}

			var src datamovestore.Store
			switch {
			case directCRDBCopy:
				src = datamovestore.NewCopyCRDBDirect(logger, conns[1].(*dbconn.PGConn).Conn)
			case gcpBucket != "":
				creds, err := google.FindDefaultCredentials(ctx)
				if err != nil {
					return err
				}
				gcpClient, err := storage.NewClient(context.Background())
				if err != nil {
					return err
				}
				src = datamovestore.NewGCPStore(logger, gcpClient, creds, gcpBucket)
			case s3Bucket != "":
				sess, err := session.NewSession()
				if err != nil {
					return err
				}
				creds, err := sess.Config.Credentials.Get()
				if err != nil {
					return err
				}
				src = datamovestore.NewS3Store(logger, sess, creds, s3Bucket)
			case localPath != "":
				src, err = datamovestore.NewLocalStore(logger, localPath)
				if err != nil {
					return err
				}
			default:
				return errors.AssertionFailedf("data source must be configured (--s3-bucket, --gcp-bucket, --direct-copy)")
			}
			if flushSize == 0 {
				flushSize = src.DefaultFlushBatchSize()
			}
			defer func() {
				if cleanup {
					if err := src.Cleanup(ctx); err != nil {
						logger.Err(err).Msgf("error marking object for cleanup")
					}
				}
			}()
			logger.Debug().
				Int("flush_size", flushSize).
				Str("store", fmt.Sprintf("%T", src)).
				Msg("initial config")

			// TODO: optimise for single table
			logger.Info().Msgf("checking database details")
			dbTables, err := dbverify.Verify(ctx, conns)
			if err != nil {
				return err
			}
			if dbTables, err = dbverify.FilterResult(cmdutil.TableFilter(), dbTables); err != nil {
				return err
			}
			for _, tbl := range dbTables.ExtraneousTables {
				logger.Warn().
					Str("table", tbl.SafeString()).
					Msgf("ignoring table as it is missing a definition on the source")
			}
			for _, tbl := range dbTables.MissingTables {
				logger.Warn().
					Str("table", tbl.SafeString()).
					Msgf("ignoring table as it is missing a definition on the target")
			}
			for _, tbl := range dbTables.Verified {
				logger.Info().
					Str("source_table", tbl[0].SafeString()).
					Str("target_table", tbl[1].SafeString()).
					Msgf("found matching table")
			}

			logger.Info().Msgf("verifying common tables")
			tables, err := tableverify.VerifyCommonTables(ctx, conns, dbTables.Verified)
			if err != nil {
				return err
			}
			logger.Info().Msgf("establishing snapshot")
			sqlSrc, err := datamove.InferExportSource(ctx, conns[0])
			if err != nil {
				return err
			}
			logger.Info().
				Int("num_tables", len(tables)).
				Str("snapshot_id", sqlSrc.SnapshotID()).
				Msgf("starting data movement")
			for _, table := range tables {
				for _, col := range table.MismatchingTableDefinitions {
					logger.Warn().
						Str("reason", col.Info).
						Msgf("not migrating column %s as it mismatches", col.Name)
				}
				if !table.RowVerifiable {
					logger.Error().Msgf("table %s do not have matching primary keys, cannot migrate", table.SafeString())
					continue
				}

				logger.Info().
					Str("table", table.SafeString()).
					Msgf("data extraction phase starting")

				startTime := time.Now()
				e, err := datamove.Export(ctx, logger, sqlSrc, src, table.VerifiedTable, flushSize)
				if err != nil {
					return err
				}
				defer func() {
					if cleanup {
						for _, r := range e.Resources {
							if err := r.MarkForCleanup(ctx); err != nil {
								logger.Err(err).Msgf("error cleaning up resource")
							}
						}
					}
				}()

				logger.Info().
					Str("table", table.SafeString()).
					Dur("duration", e.EndTime.Sub(e.StartTime)).
					Msgf("data extraction phase complete, starting data movement")

				if src.CanBeTarget() {
					if !live {
						_, err := datamove.Import(ctx, conns[1], logger, table.VerifiedTable, e.Resources)
						if err != nil {
							return err
						}
					} else {
						_, err := datamove.Copy(ctx, conns[1], logger, table.VerifiedTable, e.Resources)
						if err != nil {
							return err
						}
					}
				}

				logger.Info().
					Str("table", table.SafeString()).
					Dur("duration", time.Since(startTime)).
					Str("snapshot_id", e.SnapshotID).
					Msgf("data movement for table complete")
			}
			logger.Info().
				Str("snapshot_id", sqlSrc.SnapshotID()).
				Msgf("data movement completed")
			return nil
		},
	}

	cmd.PersistentFlags().BoolVar(
		&directCRDBCopy,
		"direct-copy",
		false,
		"whether to use direct copy mode",
	)
	cmd.PersistentFlags().BoolVar(
		&cleanup,
		"cleanup",
		false,
		"whether any file resources created should be deleted",
	)
	cmd.PersistentFlags().BoolVar(
		&live,
		"live",
		false,
		"whether the table must be queriable during data movement",
	)
	cmd.PersistentFlags().IntVar(
		&flushSize,
		"flush-size",
		0,
		"if set, size (in bytes) before the data source is flushed",
	)
	cmd.PersistentFlags().StringVar(
		&s3Bucket,
		"s3-bucket",
		"",
		"s3 bucket",
	)
	cmd.PersistentFlags().StringVar(
		&gcpBucket,
		"gcp-bucket",
		"",
		"gcp bucket",
	)
	cmd.PersistentFlags().StringVar(
		&localPath,
		"local-path",
		"",
		"path to upload files to locally",
	)
	cmdutil.RegisterDBConnFlags(cmd)
	cmdutil.RegisterLoggerFlags(cmd)
	cmdutil.RegisterNameFilterFlags(cmd)
	return cmd
}
