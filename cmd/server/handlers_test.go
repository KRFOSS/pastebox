package main

import "testing"

func TestStripANSIEscapeSequences(t *testing.T) {
	input := []byte("\x1b[31merror\x1b[0m: interface is up")
	got := stripANSIEscapeSequences(input)
	want := "error: interface is up"

	if string(got) != want {
		t.Fatalf("unexpected ANSI-stripped output: got %q want %q", got, want)
	}
}

func TestLooksLikeTextAcceptsUTF8LogOutput(t *testing.T) {
	input := []byte("en0: 상태=active, 주소=fe80::1\n")
	if !looksLikeText(input) {
		t.Fatalf("expected UTF-8 log output to be treated as text")
	}
}
