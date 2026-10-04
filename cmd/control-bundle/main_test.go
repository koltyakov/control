package main

import "testing"

func TestGitHubRepositoryRemotes(t *testing.T) {
	for _, test := range []struct{ remote, want string }{
		{"git@github.com:koltyakov/control.git", "koltyakov/control"},
		{"https://github.com/owner/fork.git", "owner/fork"},
		{"ssh://git@github.com/owner/fork.git", "owner/fork"},
		{"https://github.com/owner/fork/", "owner/fork"},
		{"https://other.example/owner/repo.git", ""},
		{"https://github.com/owner/repo/tree/main", ""},
		{"/local/repository", ""},
	} {
		t.Run(test.remote, func(t *testing.T) {
			if got := githubRepository(test.remote); got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
}
