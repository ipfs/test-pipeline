package cmd_test

import (
	"testing"
)

func XTestBuildExecGo(t *testing.T) {
	err := runSingle(t,
		"build",
		"placebo",
		"--builder",
		"exec:go",
	)

	if err != nil {
		t.Fatal(err)
	}
}

func XTestBuildDockerGo(t *testing.T) {
	// TODO: this test assumes that docker is running locally, and that we can
	// pick the .env.toml file this way, in case the user has defined a custom
	// docker endpoint. I don't think those assumptions stand.
	err := runSingle(t,
		"build",
		"placebo",
		"--builder",
		"docker:go",
	)

	if err != nil {
		t.Fatal(err)
	}
}
