package githubapp

import (
	"errors"
	"net/http"
	"testing"

	"github.com/google/go-github/v88/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeAPIBase(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "", want: "https://api.github.com"},
		{in: "https://api.github.com/", want: "https://api.github.com"},
		{in: "https://ghe.example.com", want: "https://ghe.example.com/api/v3"},
		{in: "https://ghe.example.com/", want: "https://ghe.example.com/api/v3"},
		{in: "https://ghe.example.com/api/v3/", want: "https://ghe.example.com/api/v3"},
		{in: "ftp://ghe.example.com", wantErr: true},
		{in: "://bad", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := normalizeAPIBase(tt.in)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseRepo(t *testing.T) {
	repo, err := ParseRepo(" owner/name ")
	require.NoError(t, err)
	assert.Equal(t, "owner/name", repo.String())
	assert.Equal(t, "owner", repo.Owner)
	assert.Equal(t, "name", repo.Name)

	for _, bad := range []string{"", "owner", "owner/", "/name", "a/b/c"} {
		_, err := ParseRepo(bad)
		require.Error(t, err, bad)
	}
}

func TestIsRetryable(t *testing.T) {
	retryable := &github.ErrorResponse{Response: &http.Response{StatusCode: http.StatusBadGateway}}
	assert.True(t, isRetryable(retryable))

	notRetryable := &github.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}}
	assert.False(t, isRetryable(notRetryable))

	assert.False(t, isRetryable(errors.New("boom")))
}
