package main

import (
	"fmt"
	"strings"
)

const cookbookOverrideLabel = "docker-zero.cookbook"

// Normalize Docker Hub's equivalent repository spellings without treating a
// third-party namespace or registry containing a model name as that model.
func normalizedImage(image string) string {
	image = strings.TrimSpace(image)
	image = strings.TrimPrefix(image, "index.docker.io/")
	image = strings.TrimPrefix(image, "docker.io/")
	image = strings.TrimPrefix(image, "library/")
	return image
}

func imageRepository(image string) string {
	if index := strings.IndexByte(image, '@'); index >= 0 {
		image = image[:index]
	}
	if index := strings.LastIndexByte(image, ':'); index > strings.LastIndexByte(image, '/') {
		image = image[:index]
	}
	return image
}

func imageMappingMatches(image, candidate string) bool {
	image, candidate = normalizedImage(image), normalizedImage(candidate)
	if image == candidate {
		return true
	}
	// An untagged registered repository deliberately accepts any tag/digest.
	return candidate == imageRepository(candidate) && imageRepository(image) == candidate
}

func (e *Engine) cookbookFor(_ string, image string) (*Cookbook, error) {
	for _, kind := range sortedCookbookKinds(e.cookbooks) {
		cb := e.cookbooks[kind]
		for _, candidate := range cb.ImageNames {
			if imageMappingMatches(image, candidate) {
				return cb, nil
			}
		}
	}
	return nil, fmt.Errorf("no cookbook registered for image %q; register image_names or explicitly set label %s", image, cookbookOverrideLabel)
}

func (e *Engine) containerCookbook(image string, labels map[string]string) (*Cookbook, error) {
	if override, exists := labels[cookbookOverrideLabel]; exists {
		if cb, ok := e.cookbooks[override]; ok {
			return cb, nil
		}
		return nil, fmt.Errorf("unknown explicit cookbook %q", override)
	}
	return e.cookbookFor("", image)
}
