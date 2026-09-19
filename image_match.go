package main

import (
	"fmt"
	"strings"
)

// Compare repository identities, not substrings. A repository-only matcher
// allows its tags/digests; an explicitly qualified matcher takes precedence.
func (e *Engine) cookbookFor(name, image string) (*Cookbook, error) {
	repository, qualifier := splitImageReference(image)
	var selected *Cookbook
	best := 0
	ambiguous := false
	for _, kind := range sortedCookbookKinds(e.cookbooks) {
		cb := e.cookbooks[kind]
		score := 0
		for _, candidate := range cb.ImageNames {
			candidateRepository, candidateQualifier := splitImageReference(candidate)
			if candidateRepository != repository {
				continue
			}
			if candidateQualifier == "" {
				score = max(score, 1)
			}
			effectiveQualifier := qualifier
			if effectiveQualifier == "" {
				effectiveQualifier = ":latest"
			}
			if candidateQualifier != "" && candidateQualifier == effectiveQualifier {
				score = 2
			}
		}
		if score > best {
			selected, best, ambiguous = cb, score, false
		} else if score != 0 && score == best {
			ambiguous = true
		}
	}
	if ambiguous {
		return nil, fmt.Errorf("ambiguous cookbook mapping for image %q", image)
	}
	if selected == nil {
		return nil, fmt.Errorf("no cookbook matches name=%q image=%q; add an explicit image_names mapping", name, image)
	}
	return selected, nil
}

func splitImageReference(image string) (repository, qualifier string) {
	repository = strings.TrimSpace(image)
	if index := strings.IndexByte(repository, '@'); index >= 0 {
		qualifier, repository = repository[index:], repository[:index]
	}
	if index := strings.LastIndexByte(repository, ':'); index > strings.LastIndexByte(repository, '/') {
		qualifier, repository = repository[index:]+qualifier, repository[:index]
	}
	domain, path := "docker.io", repository
	parts := strings.SplitN(repository, "/", 2)
	if len(parts) == 2 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		domain, path = parts[0], parts[1]
	}
	if domain == "index.docker.io" || domain == "registry-1.docker.io" {
		domain = "docker.io"
	}
	if domain == "docker.io" && !strings.Contains(path, "/") {
		path = "library/" + path
	}
	return domain + "/" + path, qualifier
}
