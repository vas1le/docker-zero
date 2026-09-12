package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// DiagnosticError is a user-actionable configuration error. It deliberately
// carries source coordinates and a small excerpt so cookbook mistakes cannot be
// confused with failures in the orchestrator under test.
type DiagnosticError struct {
	Code       string
	File       string
	JSONPath   string
	Message    string
	Hint       string
	Line       int
	Column     int
	SourceLine string
}

func (e *DiagnosticError) Error() string { return e.Message }

func (e *DiagnosticError) Render() string {
	var out strings.Builder
	code := e.Code
	if code == "" {
		code = "CONFIG_ERROR"
	}
	fmt.Fprintf(&out, "%s: %s\n", code, e.Message)
	if e.File != "" {
		fmt.Fprintf(&out, "  file   : %s\n", e.File)
	}
	if e.JSONPath != "" {
		fmt.Fprintf(&out, "  path   : %s\n", e.JSONPath)
	}
	if e.Line > 0 {
		fmt.Fprintf(&out, "  line   : %d\n", e.Line)
		fmt.Fprintf(&out, "  column : %d\n", e.Column)
		if e.SourceLine != "" {
			width := len(strconv.Itoa(e.Line))
			fmt.Fprintf(&out, "\n  %*d | %s\n", width, e.Line, e.SourceLine)
			fmt.Fprintf(&out, "  %*s | %s^\n", width, "", strings.Repeat(" ", max(0, e.Column-1)))
		}
	}
	if e.Hint != "" {
		fmt.Fprintf(&out, "\n  hint   : %s\n", e.Hint)
	}
	return strings.TrimRight(out.String(), "\n")
}

func renderError(err error) string {
	var diagnostic *DiagnosticError
	if errors.As(err, &diagnostic) {
		return diagnostic.Render()
	}
	return err.Error()
}

func newSourceDiagnostic(code, file, jsonPath, message, hint string, data []byte, offset int) *DiagnosticError {
	line, column, sourceLine := sourceCoordinates(data, offset)
	return &DiagnosticError{
		Code:       code,
		File:       file,
		JSONPath:   jsonPath,
		Message:    message,
		Hint:       hint,
		Line:       line,
		Column:     column,
		SourceLine: sourceLine,
	}
}

func sourceCoordinates(data []byte, offset int) (line int, column int, sourceLine string) {
	if len(data) == 0 {
		return 1, 1, ""
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= len(data) {
		offset = len(data) - 1
	}

	lineStart := bytes.LastIndexByte(data[:offset], '\n') + 1
	lineEndRelative := bytes.IndexByte(data[offset:], '\n')
	lineEnd := len(data)
	if lineEndRelative >= 0 {
		lineEnd = offset + lineEndRelative
	}
	line = bytes.Count(data[:lineStart], []byte{'\n'}) + 1
	column = utf8.RuneCount(data[lineStart:offset]) + 1
	sourceLine = strings.TrimSuffix(string(data[lineStart:lineEnd]), "\r")
	return line, column, sourceLine
}

func jsonDecodeDiagnostic(file string, data []byte, err error, decoderOffset int64, locations ...map[string]int) error {
	const hint = "correct the cookbook and run docker-zero --check --cookbook-dir <directory>"

	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return newSourceDiagnostic(
			"CONFIG_SYNTAX_ERROR",
			file,
			"",
			syntax.Error(),
			hint,
			data,
			max(0, int(syntax.Offset)-1),
		)
	}

	var typeError *json.UnmarshalTypeError
	if errors.As(err, &typeError) {
		jsonPath := typeError.Field
		if jsonPath != "" && !strings.HasPrefix(jsonPath, "/") {
			jsonPath = "/" + strings.ReplaceAll(jsonPath, ".", "/")
		}
		return newSourceDiagnostic(
			"CONFIG_TYPE_ERROR",
			file,
			jsonPath,
			typeError.Error(),
			hint,
			data,
			max(0, int(typeError.Offset)-1),
		)
	}

	const unknownPrefix = "json: unknown field "
	if strings.HasPrefix(err.Error(), unknownPrefix) {
		field, unquoteErr := strconv.Unquote(strings.TrimPrefix(err.Error(), unknownPrefix))
		if unquoteErr != nil {
			field = strings.Trim(strings.TrimPrefix(err.Error(), unknownPrefix), `"`)
		}
		offset := findJSONKeyOffsetBefore(data, field, int(decoderOffset))
		jsonPath := "/" + escapeJSONPointer(field)
		if len(locations) > 0 && locations[0] != nil {
			if candidate := closestJSONPath(locations[0], field, offset); candidate != "" {
				jsonPath = candidate
			}
		}
		return newSourceDiagnostic(
			"CONFIG_UNKNOWN_FIELD",
			file,
			jsonPath,
			fmt.Sprintf("unknown field %q; this is usually a typo", field),
			hint,
			data,
			offset,
		)
	}

	offset := int(decoderOffset)
	if offset > 0 {
		offset--
	}
	return newSourceDiagnostic("CONFIG_PARSE_ERROR", file, "", err.Error(), hint, data, offset)
}

// scanJSONDocument returns exact byte locations for JSON-pointer paths and
// rejects duplicate object keys. encoding/json otherwise silently accepts the
// last duplicate, which is unsafe for scenario definitions.
func scanJSONDocument(file string, data []byte) (map[string]int, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	locations := make(map[string]int)
	if err := scanJSONValue(decoder, file, data, "", locations); err != nil {
		return nil, err
	}
	if token, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, newSourceDiagnostic(
				"CONFIG_TRAILING_DATA",
				file,
				"",
				fmt.Sprintf("unexpected JSON token %v after the cookbook document", token),
				"remove the trailing document or data",
				data,
				int(decoder.InputOffset()),
			)
		}
		return nil, jsonDecodeDiagnostic(file, data, err, decoder.InputOffset())
	}
	return locations, nil
}

func scanJSONValue(decoder *json.Decoder, file string, data []byte, path string, locations map[string]int) error {
	token, err := decoder.Token()
	if err != nil {
		return jsonDecodeDiagnostic(file, data, err, decoder.InputOffset())
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}

	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return jsonDecodeDiagnostic(file, data, err, decoder.InputOffset())
			}
			key, ok := keyToken.(string)
			if !ok {
				return newSourceDiagnostic("CONFIG_SYNTAX_ERROR", file, path, "object key is not a string", "use quoted JSON object keys", data, int(decoder.InputOffset()))
			}
			keyOffset := jsonStringTokenStart(data, decoder.InputOffset())
			keyPath := path + "/" + escapeJSONPointer(key)
			if _, duplicate := seen[key]; duplicate {
				return nilIfDiagnostic(newSourceDiagnostic(
					"CONFIG_DUPLICATE_KEY",
					file,
					keyPath,
					fmt.Sprintf("duplicate JSON key %q; encoding/json would silently keep only the last value", key),
					"remove one of the duplicate keys",
					data,
					keyOffset,
				))
			}
			seen[key] = struct{}{}
			locations[keyPath] = keyOffset
			if err := scanJSONValue(decoder, file, data, keyPath, locations); err != nil {
				return err
			}
		}
		if _, err := decoder.Token(); err != nil {
			return jsonDecodeDiagnostic(file, data, err, decoder.InputOffset())
		}
	case '[':
		index := 0
		for decoder.More() {
			itemPath := path + "/" + strconv.Itoa(index)
			locations[itemPath] = int(decoder.InputOffset())
			if err := scanJSONValue(decoder, file, data, itemPath, locations); err != nil {
				return err
			}
			index++
		}
		if _, err := decoder.Token(); err != nil {
			return jsonDecodeDiagnostic(file, data, err, decoder.InputOffset())
		}
	default:
		return newSourceDiagnostic("CONFIG_SYNTAX_ERROR", file, path, fmt.Sprintf("unexpected delimiter %q", delimiter), "correct the JSON structure", data, int(decoder.InputOffset()))
	}
	return nil
}

// nilIfDiagnostic exists only to make duplicate-key returns visually explicit
// at the detection site.
func nilIfDiagnostic(err error) error { return err }

func jsonStringTokenStart(data []byte, endOffset int64) int {
	i := min(len(data)-1, int(endOffset)-1)
	for i >= 0 && (data[i] == ' ' || data[i] == '\t' || data[i] == '\r' || data[i] == '\n') {
		i--
	}
	if i < 0 || data[i] != '"' {
		return max(0, i)
	}
	for i--; i >= 0; i-- {
		if data[i] != '"' {
			continue
		}
		backslashes := 0
		for j := i - 1; j >= 0 && data[j] == '\\'; j-- {
			backslashes++
		}
		if backslashes%2 == 0 {
			return i
		}
	}
	return 0
}

func closestJSONPath(locations map[string]int, field string, targetOffset int) string {
	escaped := "/" + escapeJSONPointer(field)
	bestPath := ""
	bestDistance := int(^uint(0) >> 1)
	for path, offset := range locations {
		if !strings.HasSuffix(path, escaped) {
			continue
		}
		distance := offset - targetOffset
		if distance < 0 {
			distance = -distance
		}
		if distance < bestDistance {
			bestDistance = distance
			bestPath = path
		}
	}
	return bestPath
}

func findJSONKeyOffsetBefore(data []byte, key string, before int) int {
	needle := []byte(strconv.Quote(key))
	if before <= 0 || before > len(data) {
		before = len(data)
	}
	best := -1
	for start := 0; start < before; {
		index := bytes.Index(data[start:before], needle)
		if index < 0 {
			break
		}
		index += start
		after := index + len(needle)
		for after < len(data) && (data[after] == ' ' || data[after] == '\t' || data[after] == '\r' || data[after] == '\n') {
			after++
		}
		if after < len(data) && data[after] == ':' {
			best = index
		}
		start = index + len(needle)
	}
	if best >= 0 {
		return best
	}
	return findJSONKeyOffset(data, key)
}

func findJSONKeyOffset(data []byte, key string) int {
	needle := []byte(strconv.Quote(key))
	for start := 0; start < len(data); {
		index := bytes.Index(data[start:], needle)
		if index < 0 {
			break
		}
		index += start
		after := index + len(needle)
		for after < len(data) && (data[after] == ' ' || data[after] == '\t' || data[after] == '\r' || data[after] == '\n') {
			after++
		}
		if after < len(data) && data[after] == ':' {
			return index
		}
		start = index + len(needle)
	}
	return 0
}

func escapeJSONPointer(value string) string {
	value = strings.ReplaceAll(value, "~", "~0")
	return strings.ReplaceAll(value, "/", "~1")
}
