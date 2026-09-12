package main

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func FuzzStripAPIVersion(f *testing.F) {
	for _, value := range []string{"/_ping", "/v1.43/containers/json", "/v999.1/x", "", "/v1.x/test", "/v1.43"} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, value string) {
		output := stripAPIVersion(value)
		if strings.HasPrefix(value, "/") && !strings.HasPrefix(output, "/") {
			t.Fatalf("absolute input became relative: input=%q output=%q", value, output)
		}
	})
}

func FuzzReadRESPCommand(f *testing.F) {
	for _, value := range [][]byte{
		[]byte("*1\r\n$4\r\nPING\r\n"),
		[]byte("PING\r\n"),
		[]byte("*2\r\n$3\r\nGET\r\n$1\r\nx\r\n"),
		[]byte("*x\r\n"),
		{},
	} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, value []byte) {
		if len(value) > 1<<20 {
			t.Skip()
		}
		_, _ = readRESPCommand(bufio.NewReader(bytes.NewReader(value)))
	})
}

func FuzzRESPReplyValidator(f *testing.F) {
	for _, value := range []string{"+PONG\r\n", "-ERR nope\r\n", ":1\r\n", "$3\r\nabc\r\n", "*2\r\n+OK\r\n:1\r\n", ""} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, value string) {
		if len(value) > 1<<20 {
			t.Skip()
		}
		_ = validateRESPReply(value)
	})
}

func FuzzCookbookDecoderNeverPanics(f *testing.F) {
	nginx, err := embeddedCookbooks.ReadFile("cookbooks/nginx.json")
	if err != nil {
		f.Fatal(err)
	}
	f.Add(nginx)
	f.Add([]byte(`{"kind":"nginx"}`))
	f.Add([]byte(`{"kind":`))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, value []byte) {
		if len(value) > 2<<20 {
			t.Skip()
		}
		_, _ = decodeCookbook("<fuzz>/cookbook.json", value)
	})
}
