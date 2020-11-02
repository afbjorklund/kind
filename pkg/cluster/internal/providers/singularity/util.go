/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package singularity

import (
	"strings"

	"k8s.io/apimachinery/pkg/util/version"

	"sigs.k8s.io/kind/pkg/errors"
	"sigs.k8s.io/kind/pkg/exec"
)

// IsAvailable checks if singularity is available in the system
func IsAvailable() bool {
	cmd := exec.Command("singularity", "--version")
	lines, err := exec.OutputLines(cmd)
	if err != nil || len(lines) != 1 {
		return false
	}
	return strings.HasPrefix(lines[0], "singularity version")
}

func getSingularityVersion() (*version.Version, error) {
	cmd := exec.Command("singularity", "--version")
	lines, err := exec.OutputLines(cmd)
	if err != nil {
		return nil, err
	}

	// output is like `3.5.0`
	if len(lines) != 1 {
		return nil, errors.Errorf("version should only be one line, got %d", len(lines))
	}
	parts := strings.Split(lines[0], " ")
	if len(parts) != 3 {
		return nil, errors.Errorf("singularity --version contents should have 3 parts, got %q", lines[0])
	}
	return version.ParseSemantic(parts[2])
}

const (
	minSupportedVersion = "3.5.0"
)

func ensureMinVersion() error {
	// ensure that singularity version is a compatible version
	v, err := getSingularityVersion()
	if err != nil {
		return errors.Wrap(err, "failed to check singularity version")
	}
	if !v.AtLeast(version.MustParseSemantic(minSupportedVersion)) {
		return errors.Errorf("singularity version %q is too old, please upgrade to %q or later", v, minSupportedVersion)
	}
	return nil
}
