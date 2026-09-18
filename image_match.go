package main

import "strings"

// A bare repository mapping covers its tags/digests. An explicit tag or digest
// only matches that exact reference and takes precedence over a repository-wide
// mapping. Container names and repository substrings never select a service.
func imageMatchScore(candidate, image string) int {
	candidateRepo, candidateSelector := splitImageReference(candidate)
	imageRepo, imageSelector := splitImageReference(image)
	if candidateRepo == "" || candidateRepo != imageRepo {
		return 0
	}
	if candidateSelector == "" {
		return 1
	}
	if candidateSelector == imageSelector {
		return 2
	}
	return 0
}

func splitImageReference(image string) (string, string) {
	image = strings.TrimSpace(image)
	selector := ""
	if at := strings.IndexByte(image, '@'); at >= 0 {
		selector = image[at:]
		image = image[:at]
	}
	// Only a colon after the last slash is a tag, not a registry port.
	if colon := strings.LastIndexByte(image, ':'); colon > strings.LastIndexByte(image, '/') {
		if selector == "" {
			selector = image[colon:]
		}
		image = image[:colon]
	}
	parts := strings.SplitN(image, "/", 2)
	registry := "docker.io"
	repository := image
	if len(parts) == 2 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		registry, repository = parts[0], parts[1]
	}
	if registry == "index.docker.io" {
		registry = "docker.io"
	}
	if registry == "docker.io" && !strings.Contains(repository, "/") {
		repository = "library/" + repository
	}
	return registry + "/" + repository, selector
}
