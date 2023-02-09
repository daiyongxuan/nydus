// Copyright 2023 Nydus Developers. All rights reserved.
//
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/dragonflyoss/image-service/smoke/tests/tool"
	"github.com/dragonflyoss/image-service/smoke/tests/tool/test"
)

const (
	paramZran = "zran"
)

type ImageTestSuite struct {
	T              *testing.T
	preparedImages map[string]string
}

func (i *ImageTestSuite) TestConvertImages() test.Generator {

	scenarios := tool.DescartesIterator{}
	scenarios.
		Dimension(paramImage, []interface{}{"nginx:latest"}).
		Dimension(paramFSVersion, []interface{}{"5", "6"}).
		Dimension(paramZran, []interface{}{false, true}).
		Skip(
			func(param *tool.DescartesItem) bool {
				// Zran not work with rafs v6.
				return param.GetString(paramFSVersion) == "5" && param.GetBool(paramZran)
			})

	return func() (name string, testCase test.Case) {
		if !scenarios.HasNext() {
			return
		}
		scenario := scenarios.Next()

		ctx := tool.DefaultContext(i.T)
		ctx.Build.FSVersion = scenario.GetString(paramFSVersion)
		ctx.Build.OCIRef = scenario.GetBool(paramZran)

		image := i.prepareImage(i.T, scenario.GetString(paramImage))
		return scenario.Str(), func(t *testing.T) {
			i.TestConvertImage(t, *ctx, image)
		}
	}
}

func (i *ImageTestSuite) TestConvertImage(t *testing.T, ctx tool.Context, source string) {

	// Prepare work directory
	ctx.PrepareWorkDir(t)
	defer ctx.Destroy(t)

	// Prepare options
	ociRefSuffix := ""
	enableOCIRef := ""
	if ctx.Build.OCIRef {
		ociRefSuffix = "-oci-ref"
		enableOCIRef = "--oci-ref"
	}
	target := fmt.Sprintf("%s-nydus-v%s%s", source, ctx.Build.FSVersion, ociRefSuffix)
	fsVersion := fmt.Sprintf("--fs-version %s", ctx.Build.FSVersion)
	if ctx.Binary.NydusifyOnlySupportV5 {
		fsVersion = ""
	}
	compressor := "--compressor lz4_block"
	if ctx.Binary.NydusifyNotSupportCompressor {
		compressor = ""
	}

	// Convert image
	convertCmd := fmt.Sprintf(
		"%s convert --source %s --target %s %s %s --nydus-image %s --work-dir %s %s",
		ctx.Binary.Nydusify, source, target, fsVersion, enableOCIRef, ctx.Binary.Builder, ctx.Env.WorkDir, compressor,
	)
	tool.Run(t, convertCmd)

	// Check image
	nydusifyPath := ctx.Binary.Nydusify
	if ctx.Binary.NydusifyChecker != "" {
		nydusifyPath = ctx.Binary.NydusifyChecker
	}
	checkCmd := fmt.Sprintf(
		"%s check --source %s --target %s --nydus-image %s --nydusd %s --work-dir %s",
		nydusifyPath, source, target, ctx.Binary.Builder, ctx.Binary.Nydusd, filepath.Join(ctx.Env.WorkDir, "check"),
	)
	tool.Run(t, checkCmd)
}

func (i *ImageTestSuite) prepareImage(t *testing.T, image string) string {
	if i.preparedImages == nil {
		i.preparedImages = make(map[string]string)
	}
	loc, ok := i.preparedImages[image]
	if !ok {
		loc = tool.PrepareImage(t, image)
		i.preparedImages[image] = loc
	}
	return loc
}

func TestImage(t *testing.T) {
	test.Run(t, &ImageTestSuite{T: t})
}