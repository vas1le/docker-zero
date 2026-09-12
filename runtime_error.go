package main

import (
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
)

// InternalPanicError distinguishes a docker-zero implementation defect from a
// failure deliberately produced by a cookbook. The process exits with
// exitRuntime after reporting this error, so the matrix harness cannot mistake
// a broken mock for an orchestrator defect.
type InternalPanicError struct {
	Component string
	Recovered any
	Stack     string
}

func (e *InternalPanicError) Error() string {
	return fmt.Sprintf("%s panicked: %v\n%s", e.Component, e.Recovered, strings.TrimSpace(e.Stack))
}

func (e *Engine) Errors() <-chan error {
	if e == nil {
		return nil
	}
	return e.internalErrors
}

func (e *Engine) reportInternalPanic(component string, recovered any) {
	if e == nil {
		return
	}
	panicErr := &InternalPanicError{
		Component: component,
		Recovered: recovered,
		Stack:     string(debug.Stack()),
	}
	if e.ledger != nil {
		e.ledger.Log(LedgerEntry{
			Channel: "docker-zero.internal",
			Event:   "panic",
			Request: map[string]any{"component": component},
			Response: map[string]any{
				"panic": fmt.Sprint(recovered),
				"stack": panicErr.Stack,
			},
		})
	}
	reportRuntimeError(e.internalErrors, panicErr)
}

func recoverHTTPHandler(engine *Engine, component string, writer http.ResponseWriter) {
	if recovered := recover(); recovered != nil {
		engine.reportInternalPanic(component, recovered)
		// A handler may already have committed a response. In that case net/http
		// ignores the duplicate WriteHeader, but the engine still terminates and
		// preserves the panic in its ledger/log instead of silently continuing.
		writer.Header().Set("Connection", "close")
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		writer.WriteHeader(http.StatusInternalServerError)
		_, _ = writer.Write([]byte("docker-zero internal error; inspect engine.log\n"))
	}
}

func recoverDockerHTTPHandler(engine *Engine, writer http.ResponseWriter) {
	if recovered := recover(); recovered != nil {
		engine.reportInternalPanic("Docker API", recovered)
		writer.Header().Set("Connection", "close")
		writeDockerError(writer, http.StatusInternalServerError, "docker-zero internal error; inspect engine.log")
	}
}

func recoverTCPHandler(engine *Engine, component string) {
	if recovered := recover(); recovered != nil {
		engine.reportInternalPanic(component, recovered)
	}
}
