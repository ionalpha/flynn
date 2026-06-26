package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunModelInspectCatalog(t *testing.T) {
	var out bytes.Buffer
	if err := runModelInspect([]string{"qwen2.5:0.5b-instruct"}, t.TempDir(), &out); err != nil {
		t.Fatalf("inspect: %v", err)
	}
	s := out.String()
	for _, want := range []string{"trusted", "pinned digest", "no network", "runs it"} {
		if !strings.Contains(s, want) {
			t.Fatalf("inspect output missing %q:\n%s", want, s)
		}
	}
}

func TestRunModelInspectRefusesCodeExecAndReports(t *testing.T) {
	var out bytes.Buffer
	if err := runModelInspect([]string{"/tmp/model.bin"}, t.TempDir(), &out); err != nil {
		t.Fatalf("inspect should report, not error: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "untrusted") || !strings.Contains(s, "format:    refused") {
		t.Fatalf("inspect of a pickle must show untrusted and a refused format:\n%s", s)
	}
	if !strings.Contains(s, "REFUSE") {
		t.Fatalf("inspect must report this host would refuse to run it:\n%s", s)
	}
}

func TestRunModelInspectNeedsArg(t *testing.T) {
	var out bytes.Buffer
	if err := runModelInspect(nil, t.TempDir(), &out); err == nil {
		t.Fatal("inspect with no argument must error")
	}
}
