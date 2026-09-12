package main

import (
	"context"
	"io"
	"testing"
)

func testClose(t *testing.T, closer io.Closer) {
	t.Helper()
	if err := closer.Close(); err != nil {
		t.Errorf("close %T: %v", closer, err)
	}
}

func testCloseLedger(t *testing.T, ledger *Ledger) {
	t.Helper()
	if err := ledger.Close(); err != nil {
		t.Errorf("close ledger: %v", err)
	}
}

func testCloseRuntimes(t *testing.T, engine *Engine) {
	t.Helper()
	if err := engine.closeRuntimes(context.Background()); err != nil {
		t.Errorf("close runtimes: %v", err)
	}
}

func testCloseRedisMock(t *testing.T, mock *RedisMock) {
	t.Helper()
	if err := mock.Close(context.Background()); err != nil {
		t.Errorf("close Redis mock: %v", err)
	}
}
