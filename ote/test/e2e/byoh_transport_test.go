package winc

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func TestWaitForHostProcessOutputRetriesEmpty(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	calls := 0
	output, err := waitForHostProcessOutput(ctx, func(context.Context) ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, nil
		}
		return []byte("BYOH_START\nresult\nBYOH_COMPLETE"), nil
	}, "BYOH_START", "BYOH_COMPLETE")
	if err != nil || !bytes.Contains(output.output, []byte("result")) || calls < 2 {
		t.Fatalf("empty output was not retried: calls=%d output=%q err=%v", calls, output.output, err)
	}
}

func TestWaitForHostProcessOutputRetriesPartial(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	calls := 0
	output, err := waitForHostProcessOutput(ctx, func(context.Context) ([]byte, error) {
		calls++
		if calls == 1 {
			return []byte("BYOH_START"), nil
		}
		return []byte("BYOH_START\nresult\nBYOH_COMPLETE"), nil
	}, "BYOH_START", "BYOH_COMPLETE")
	if err != nil || !bytes.Contains(output.output, []byte("result")) || calls < 2 {
		t.Fatalf("partial output was not retried: calls=%d output=%q err=%v", calls, output.output, err)
	}
}

func TestWaitForHostProcessOutputTimesOut(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := waitForHostProcessOutput(ctx, func(context.Context) ([]byte, error) {
		return nil, nil
	}, "BYOH_START", "BYOH_COMPLETE")
	if err == nil || !strings.Contains(err.Error(), "BYOH_START=false,BYOH_COMPLETE=false") ||
		!strings.Contains(err.Error(), "attempts=1 bytes=0 lastError=none") {
		t.Fatalf("permanently empty output was accepted or lacked diagnostics: %v", err)
	}
}

func TestWaitForHostProcessOutputTimesOutPartial(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	output, err := waitForHostProcessOutput(ctx, func(context.Context) ([]byte, error) {
		return []byte("BYOH_START"), nil
	}, "BYOH_START", "BYOH_COMPLETE")
	if err == nil || !strings.Contains(err.Error(), "BYOH_START=true,BYOH_COMPLETE=false") ||
		!strings.Contains(err.Error(), "attempts=1 bytes=10 lastError=none") || string(output.output) != "BYOH_START" {
		t.Fatalf("partial output was accepted or lacked diagnostics: output=%q err=%v", output.output, err)
	}
}

func TestEncodePowerShellScript(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
	}{
		{name: "configured command", script: lifecycleConfiguredCommand},
		{name: "CRLF multiline", script: "$ErrorActionPreference='Stop';\r\nWrite-Output 'line one';\r\nWrite-Output 'line two'"},
		{name: "non-ASCII", script: "Write-Output '检查编码𝄞'"},
	} {
		encoded := encodePowerShellScript(tc.script)
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(decoded)%2 != 0 {
			t.Fatalf("%s: encoded script is not UTF-16LE: %q", tc.name, encoded)
		}
		units := make([]uint16, len(decoded)/2)
		for i := range units {
			units[i] = uint16(decoded[2*i]) | uint16(decoded[2*i+1])<<8
		}
		if got := string(utf16.Decode(units)); got != tc.script {
			t.Fatalf("%s: encoded script did not round-trip exactly: got=%q want=%q", tc.name, got, tc.script)
		}
	}
}

func TestHostProcessCommandFingerprint(t *testing.T) {
	command := []string{"powershell.exe", "-Command", "Write-Output 'test'"}
	first := hostProcessCommandFingerprint(command)
	if first == "" || first != hostProcessCommandFingerprint(command) {
		t.Fatalf("command fingerprint is not stable: %q", first)
	}
	if first == hostProcessCommandFingerprint([]string{"powershell.exe", "-Command", "Write-Output 'changed'"}) {
		t.Fatal("distinct commands produced the same fingerprint")
	}
}
