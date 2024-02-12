package datablobstorage

import (
	"context"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2/google"
)

func TestGCPResource_ImportURL(t *testing.T) {
	for _, tc := range []struct {
		desc     string
		r        *gcpResource
		expected string
	}{
		{
			desc: "basic test",
			r: &gcpResource{
				key: "asdf/ghjk.csv",
				store: &gcpStore{
					bucket: "nangs",
					creds: &google.Credentials{
						JSON: []byte(`{"a":b}`),
					},
				},
			},
			expected: "gs://nangs/asdf/ghjk.csv?CREDENTIALS=eyJhIjpifQ==",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			u, err := tc.r.ImportURL()
			require.NoError(t, err)
			require.Equal(t, tc.expected, u)
		})
	}
}

func TestListFromContinuationPointGCP(t *testing.T) {
	t.Run("number of rows is specified leads to correct number of resources", func(t *testing.T) {
		// Set up the mock expected return
		gcpClient := &gcpClientMock{}
		bucketMock := &gcpBucketMock{}
		ctx := context.Background()
		gcpClient.On("Bucket", mock.Anything).Return(bucketMock)
		bucketMock.On("Objects", ctx, mock.Anything).
			Return(&gcpObjectITMock{
				i: 0,
				next: []storage.ObjectAttrs{
					{Name: "part_00000004.tar.gz", Metadata: map[string]string{numRowsKeyGCP: "10"}},
					{Name: "part_00000005.tar.gz", Metadata: map[string]string{numRowsKeyGCP: "10"}},
					{Name: "part_00000006.tar.gz", Metadata: map[string]string{numRowsKeyGCP: "10"}},
					{Name: "part_00000007.tar.gz", Metadata: map[string]string{numRowsKeyGCP: "10"}},
					{Name: "part_00000008.tar.gz", Metadata: map[string]string{numRowsKeyGCP: "10"}},
				},
			})

		gcpStore := gcpStore{
			bucket: "fetch-test",
			creds: &google.Credentials{
				JSON: []byte(`{"a":b}`),
			},
		}
		resources, err := listFromContinuationPointGCP(ctx, gcpClient, "part_00000004.tar.gz", "public.inventory", gcpStore.bucket, nil /* gcpStore */)
		require.NoError(t, err)
		require.Len(t, resources, 5)
	})

	t.Run("number of rows not specified leads to correct number of resources", func(t *testing.T) {
		// Set up the mock expected return
		gcpClient := &gcpClientMock{}
		bucketMock := &gcpBucketMock{}
		ctx := context.Background()
		var sb strings.Builder
		gcpClient.On("Bucket", mock.Anything).Return(bucketMock)
		bucketMock.On("Objects", ctx, mock.Anything).
			Return(&gcpObjectITMock{
				i: 0,
				next: []storage.ObjectAttrs{
					{Name: "part_00000004.tar.gz", Metadata: map[string]string{}},
					{Name: "part_00000005.tar.gz", Metadata: map[string]string{}},
					{Name: "part_00000006.tar.gz", Metadata: map[string]string{}},
					{Name: "part_00000007.tar.gz", Metadata: map[string]string{}},
					{Name: "part_00000008.tar.gz", Metadata: map[string]string{}},
				},
			})

		gcpStore := gcpStore{
			bucket: "fetch-test",
			creds: &google.Credentials{
				JSON: []byte(`{"a":b}`),
			},
			logger: zerolog.New(&sb),
		}
		resources, err := listFromContinuationPointGCP(ctx, gcpClient, "part_00000004.tar.gz", "public.inventory", gcpStore.bucket, &gcpStore /* gcpStore */)
		require.NoError(t, err)
		require.Len(t, resources, 5)
		require.Contains(t, sb.String(), "failed to find metadata")
	})
}
