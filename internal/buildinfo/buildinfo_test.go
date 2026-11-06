package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestCommitFromSettings(t *testing.T) {
	tests := []struct {
		name     string
		settings []debug.BuildSetting
		want     string
	}{
		{
			name:     "no commit information",
			settings: nil,
			want:     "unknown",
		},
		{
			name: "clean build",
			settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "abc123"},
				{Key: "vcs.modified", Value: "false"},
			},
			want: "abc123",
		},
		{
			name: "build with uncommitted changes",
			settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "abc123"},
				{Key: "vcs.modified", Value: "true"},
			},
			want: "abc123-modified",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := commitFromSettings(tt.settings); got != tt.want {
				t.Errorf("commitFromSettings() = %q, want %q", got, tt.want)
			}
		})
	}
}
