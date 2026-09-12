package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

func validateResponseOrderHTTP(path string, items []HTTPResponse) error {
	last := 0
	for index, item := range items {
		if item.From < 1 {
			return validationError(fmt.Sprintf("%s/%d/from", path, index), "response from must be >= 1")
		}
		if item.From < last {
			return validationError(fmt.Sprintf("%s/%d/from", path, index), "response from values must be non-decreasing")
		}
		last = item.From
	}
	if items[0].From != 1 {
		return validationError(path+"/0/from", "the first response must start from request 1")
	}
	return nil
}

func validateResponseOrderRedis(path string, items []RedisResponse) error {
	last := 0
	for index, item := range items {
		if item.From < 1 {
			return validationError(fmt.Sprintf("%s/%d/from", path, index), "response from must be >= 1")
		}
		if item.From < last {
			return validationError(fmt.Sprintf("%s/%d/from", path, index), "response from values must be non-decreasing")
		}
		last = item.From
	}
	if items[0].From != 1 {
		return validationError(path+"/0/from", "the first response must start from request 1")
	}
	return nil
}

func validateRESPReply(value string) error {
	reader := bufio.NewReader(strings.NewReader(value))
	frames := 0
	for {
		_, err := reader.Peek(1)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if err := parseRESPFrame(reader, 0); err != nil {
			return err
		}
		frames++
	}
	if frames == 0 {
		return fmt.Errorf("reply is empty")
	}
	return nil
}

func parseRESPFrame(reader *bufio.Reader, depth int) error {
	if depth > 64 {
		return fmt.Errorf("nesting exceeds 64 levels")
	}
	prefix, err := reader.ReadByte()
	if err != nil {
		return err
	}
	switch prefix {
	case '+', '-':
		line, err := readRESPLine(reader)
		if err != nil {
			return err
		}
		if bytes.IndexByte(line, 0) >= 0 {
			return fmt.Errorf("simple string contains NUL")
		}
		return nil
	case ':':
		line, err := readRESPLine(reader)
		if err != nil {
			return err
		}
		if _, err := strconv.ParseInt(string(line), 10, 64); err != nil {
			return fmt.Errorf("invalid integer reply")
		}
		return nil
	case '$':
		line, err := readRESPLine(reader)
		if err != nil {
			return err
		}
		length, err := strconv.Atoi(string(line))
		if err != nil || length < -1 || length > 16<<20 {
			return fmt.Errorf("invalid bulk string length")
		}
		if length == -1 {
			return nil
		}
		payload := make([]byte, length+2)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return fmt.Errorf("truncated bulk string")
		}
		if payload[length] != '\r' || payload[length+1] != '\n' {
			return fmt.Errorf("bulk string lacks CRLF terminator")
		}
		return nil
	case '*':
		line, err := readRESPLine(reader)
		if err != nil {
			return err
		}
		count, err := strconv.Atoi(string(line))
		if err != nil || count < -1 || count > 1024 {
			return fmt.Errorf("invalid array length")
		}
		if count == -1 {
			return nil
		}
		for i := 0; i < count; i++ {
			if err := parseRESPFrame(reader, depth+1); err != nil {
				return fmt.Errorf("array item %d: %w", i, err)
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported RESP2 prefix %q", prefix)
	}
}

func readRESPLine(reader *bufio.Reader) ([]byte, error) {
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("truncated line")
	}
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return nil, fmt.Errorf("line lacks CRLF terminator")
	}
	return line[:len(line)-2], nil
}

func isZeroStatePatch(patch StatePatch) bool {
	return patch == (StatePatch{})
}

func selectHTTPResponse(items []HTTPResponse, count int, status, health string) (HTTPResponse, bool) {
	var selected HTTPResponse
	found := false
	for _, item := range items {
		if count < item.From {
			continue
		}
		if item.WhenStatus != "" && item.WhenStatus != status {
			continue
		}
		if item.WhenHealth != "" && item.WhenHealth != health {
			continue
		}
		selected = item
		found = true
	}
	return selected, found
}

func selectRedisResponse(items []RedisResponse, count int, status, health string) (RedisResponse, bool) {
	var selected RedisResponse
	found := false
	for _, item := range items {
		if count < item.From {
			continue
		}
		if item.WhenStatus != "" && item.WhenStatus != status {
			continue
		}
		if item.WhenHealth != "" && item.WhenHealth != health {
			continue
		}
		selected = item
		found = true
	}
	return selected, found
}

func sortedCookbookKinds(cookbooks map[string]*Cookbook) []string {
	kinds := make([]string, 0, len(cookbooks))
	for kind := range cookbooks {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}
