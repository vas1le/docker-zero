package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func loadCookbooks(externalDir string) (map[string]*Cookbook, error) {
	result := make(map[string]*Cookbook)
	originByKind := make(map[string]string)
	var source fs.FS = embeddedCookbooks
	root := "cookbooks"
	displayRoot := "<embedded>/cookbooks"
	if strings.TrimSpace(externalDir) != "" {
		absolute, err := filepath.Abs(externalDir)
		if err != nil {
			return nil, fmt.Errorf("resolve cookbook directory %q: %w", externalDir, err)
		}
		source = os.DirFS(absolute)
		root = "."
		displayRoot = absolute
	}

	entries, err := fs.ReadDir(source, root)
	if err != nil {
		return nil, fmt.Errorf("read cookbook directory %s: %w", displayRoot, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		path := filepath.ToSlash(filepath.Join(root, entry.Name()))
		displayPath := filepath.Join(displayRoot, entry.Name())
		data, err := fs.ReadFile(source, path)
		if err != nil {
			return nil, fmt.Errorf("read cookbook %s: %w", displayPath, err)
		}
		cb, err := decodeCookbook(displayPath, data)
		if err != nil {
			return nil, err
		}
		kindKey := strings.ToLower(cb.Kind)
		if previous, exists := originByKind[kindKey]; exists {
			return nil, &DiagnosticError{
				Code:     "CONFIG_DUPLICATE_COOKBOOK",
				File:     displayPath,
				JSONPath: "/kind",
				Message:  fmt.Sprintf("cookbook kind %q is already defined by %s", cb.Kind, previous),
				Hint:     "keep exactly one cookbook file for each service kind",
			}
		}
		originByKind[kindKey] = displayPath
		result[kindKey] = cb
	}
	if len(result) == 0 {
		return nil, &DiagnosticError{Code: "CONFIG_NO_COOKBOOKS", File: displayRoot, Message: "no .json cookbook files were found", Hint: "provide nginx.json and redis.json or omit --cookbook-dir to use the embedded cookbooks"}
	}
	if err := validateCookbookSet(result, originByKind); err != nil {
		return nil, err
	}
	return result, nil
}

func decodeCookbook(path string, data []byte) (*Cookbook, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, newSourceDiagnostic("CONFIG_EMPTY_FILE", path, "", "cookbook file is empty", "restore a valid JSON cookbook", data, 0)
	}
	if bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}) {
		return nil, newSourceDiagnostic("CONFIG_UTF8_BOM", path, "", "UTF-8 BOM is not accepted in JSON cookbooks", "save the file as UTF-8 without BOM", data, 0)
	}

	locations, err := scanJSONDocument(path, data)
	if err != nil {
		return nil, err
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var cb Cookbook
	if err := decoder.Decode(&cb); err != nil {
		return nil, jsonDecodeDiagnostic(path, data, err, decoder.InputOffset(), locations)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, newSourceDiagnostic("CONFIG_TRAILING_DATA", path, "", "a second JSON value follows the cookbook document", "remove the trailing JSON value", data, int(decoder.InputOffset()))
		}
		return nil, jsonDecodeDiagnostic(path, data, err, decoder.InputOffset(), locations)
	}
	if err := cb.validate(); err != nil {
		var validation *ValidationError
		if !asValidationError(err, &validation) {
			return nil, err
		}
		offset, ok := locations[validation.Path]
		if !ok {
			offset = findJSONKeyOffset(data, lastJSONPointerSegment(validation.Path))
		}
		return nil, newSourceDiagnostic(
			"CONFIG_VALIDATION_ERROR",
			path,
			validation.Path,
			validation.Message,
			"correct the value and run docker-zero --check --cookbook-dir <directory>",
			data,
			offset,
		)
	}
	return &cb, nil
}

func asValidationError(err error, target **ValidationError) bool {
	value, ok := err.(*ValidationError)
	if ok {
		*target = value
	}
	return ok
}

func lastJSONPointerSegment(path string) string {
	if path == "" {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	value := parts[len(parts)-1]
	value = strings.ReplaceAll(value, "~1", "/")
	return strings.ReplaceAll(value, "~0", "~")
}

func validateCookbookSet(cookbooks map[string]*Cookbook, origins map[string]string) error {
	for _, required := range []string{"nginx", "redis"} {
		if _, ok := cookbooks[required]; !ok {
			return &DiagnosticError{
				Code:    "CONFIG_MISSING_COOKBOOK",
				File:    commonCookbookRoot(origins),
				Message: fmt.Sprintf("required %s cookbook is missing", required),
				Hint:    fmt.Sprintf("add %s.json or omit --cookbook-dir to use the embedded cookbooks", required),
			}
		}
	}
	for kind := range cookbooks {
		if kind != "nginx" && kind != "redis" {
			return &DiagnosticError{Code: "CONFIG_UNSUPPORTED_COOKBOOK", File: origins[kind], JSONPath: "/kind", Message: fmt.Sprintf("cookbook kind %q is not supported by docker-zero 0.3", kind), Hint: "this release supports exactly nginx and redis"}
		}
	}

	names := make(map[string]string)
	endpoints := make(map[string]string)
	images := make(map[string]string)
	for kind, cb := range cookbooks {
		origin := origins[kind]
		nameKey := strings.ToLower(cb.Defaults.Name)
		if previous, exists := names[nameKey]; exists {
			return &DiagnosticError{Code: "CONFIG_DUPLICATE_CONTAINER_NAME", File: origin, JSONPath: "/defaults/name", Message: fmt.Sprintf("default container name %q is already used by %s", cb.Defaults.Name, previous), Hint: "use a unique defaults.name per cookbook"}
		}
		names[nameKey] = kind
		endpoint := net.JoinHostPort(cb.Defaults.IPAddress, strconv.Itoa(cb.Defaults.ContainerPort))
		if previous, exists := endpoints[endpoint]; exists {
			return &DiagnosticError{Code: "CONFIG_DUPLICATE_ENDPOINT", File: origin, JSONPath: "/defaults/ip_address", Message: fmt.Sprintf("container endpoint %s is already used by %s", endpoint, previous), Hint: "assign a unique loopback IP and port to each cookbook"}
		}
		endpoints[endpoint] = kind
		for _, image := range append([]string{cb.Defaults.Image}, cb.ImageNames...) {
			key := strings.ToLower(strings.TrimSpace(image))
			if key == "" {
				continue
			}
			if previous, exists := images[key]; exists && previous != kind {
				return &DiagnosticError{Code: "CONFIG_AMBIGUOUS_IMAGE", File: origin, JSONPath: "/image_names", Message: fmt.Sprintf("image matcher %q is also owned by %s", image, previous), Hint: "image names must map to only one cookbook"}
			}
			images[key] = kind
		}
	}
	return nil
}

func commonCookbookRoot(origins map[string]string) string {
	for _, origin := range origins {
		return filepath.Dir(origin)
	}
	return ""
}
