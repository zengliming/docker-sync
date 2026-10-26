package main

import "testing"

func TestBuildVersion(t *testing.T) {
	origVersion, origCommit := version, commit
	t.Cleanup(func() { version, commit = origVersion, origCommit })

	cases := []struct {
		name    string
		version string
		commit  string
		want    string
	}{
		{name: "full sha is shortened", version: "0.1.2", commit: "1cdc7589a1b2c3d4e5f60718293a4b5c6d7e8f90", want: "0.1.2+1cdc758"},
		{name: "short sha is kept", version: "0.1.2", commit: "ef8b472", want: "0.1.2+ef8b472"},
		{name: "empty commit falls back to the vcs stamp or stays plain", version: "dev", commit: "", want: "dev"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			version, commit = tc.version, tc.commit
			got := buildVersion()
			if tc.commit == "" {
				// A build stamped from a git checkout appends its own revision,
				// so only the prefix is deterministic here.
				if got != "dev" && len(got) > len("dev") && got[:4] != "dev+" {
					t.Fatalf("buildVersion() = %q", got)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("buildVersion() = %q, want %q", got, tc.want)
			}
		})
	}
}
