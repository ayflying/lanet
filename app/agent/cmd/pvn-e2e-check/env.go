package main

import "os"

func osEnviron() []string {
	return os.Environ()
}

func repoRoot() string {
	// e2e 程序位于 <repo>/cmd/pvn-e2e-check，向上两级即仓库根。
	return "../.."
}
