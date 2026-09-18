package main

import (
	"errors"
	"fmt"
	"strings"
)

var errImageMapping = errors.New("image has no unambiguous cookbook mapping")

// Split only a tag on the final path component, never a registry's port.
// A repository-only matcher covers tags/digests of that repository; a matcher
// with a tag or digest covers only that exact reference. Docker Hub's official
// short, library/, and fully-qualified forms are aliases, not substring matches.
func splitImageReference(ref string) (repository, suffix string) {
	ref = strings.TrimSpace(ref)
	if at := strings.IndexByte(ref, '@'); at >= 0 {
		suffix, ref = ref[at:], ref[:at]
		if colon := strings.LastIndexByte(ref, ':'); colon > strings.LastIndexByte(ref, '/') {
			ref = ref[:colon]
		}
	} else if colon := strings.LastIndexByte(ref, ':'); colon > strings.LastIndexByte(ref, '/') {
		suffix, ref = ref[colon:], ref[:colon]
	}
	ref = strings.TrimPrefix(ref, "index.docker.io/")
	ref = strings.TrimPrefix(ref, "docker.io/")
	if !strings.Contains(ref, "/") {
		ref = "library/" + ref
	}
	return ref, suffix
}

func (e *Engine) cookbookFor(_ string, image string) (*Cookbook, error) {
	repository, suffix := splitImageReference(image)
	var match *Cookbook
	for _, kind := range sortedCookbookKinds(e.cookbooks) {
		cb := e.cookbooks[kind]
		for _, candidate := range append([]string{cb.Defaults.Image}, cb.ImageNames...) {
			candidateRepository, candidateSuffix := splitImageReference(candidate)
			matches := repository == candidateRepository && (candidateSuffix == "" || suffix == candidateSuffix)
			// Image inspect advertises these deterministic model IDs.
			matches = matches || image == imageID(candidate)
			if !matches {
				continue
			}
			if match != nil && match != cb {
				return nil, fmt.Errorf("%w: %q matches both %s and %s", errImageMapping, image, match.Kind, cb.Kind)
			}
			match = cb
		}
	}
	if match == nil {
		return nil, fmt.Errorf("%w: %q; register the image in cookbook image_names", errImageMapping, image)
	}
	return match, nil
}
