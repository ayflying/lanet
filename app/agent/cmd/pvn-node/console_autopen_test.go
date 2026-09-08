package main

import "testing"

func TestShouldAutoOpenConsole(t *testing.T) {
	if !shouldAutoOpenConsole(true) {
		t.Fatal("normal startup should allow automatic console opening")
	}
	if shouldAutoOpenConsole(false) {
		t.Fatal("subsequent startup should not open another console tab")
	}
}
