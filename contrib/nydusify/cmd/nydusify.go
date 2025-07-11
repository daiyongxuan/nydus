// Copyright 2020 Ant Group. All rights reserved.
//
// SPDX-License-Identifier: Apache-2.0

// The Nydusify CLI tool converts an OCI container image from source registry into
// a Nydus image using `nydus-image` CLI layer by layer, then pushes Nydus image to
// target registry.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/dragonflyoss/nydus/contrib/nydusify/pkg/optimizer"

	"github.com/containerd/containerd/reference/docker"
	"github.com/distribution/reference"
	"github.com/dustin/go-humanize"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"

	"github.com/dragonflyoss/nydus/contrib/nydusify/pkg/checker"
	"github.com/dragonflyoss/nydus/contrib/nydusify/pkg/chunkdict/generator"
	"github.com/dragonflyoss/nydus/contrib/nydusify/pkg/committer"
	"github.com/dragonflyoss/nydus/contrib/nydusify/pkg/converter"
	"github.com/dragonflyoss/nydus/contrib/nydusify/pkg/copier"
	"github.com/dragonflyoss/nydus/contrib/nydusify/pkg/packer"
	"github.com/dragonflyoss/nydus/contrib/nydusify/pkg/provider"
	"github.com/dragonflyoss/nydus/contrib/nydusify/pkg/utils"
	"github.com/dragonflyoss/nydus/contrib/nydusify/pkg/viewer"
)

var (
	revision   string
	buildTime  string
	gitVersion string
)

var maxCacheMaxRecords uint = 200

const defaultLogLevel = logrus.InfoLevel

func isPossibleValue(excepted []string, value string) bool {
	for _, v := range excepted {
		if value == v {
			return true
		}
	}
	return false
}

// This only works for OSS backend right now
func parseBackendConfig(backendConfigJSON, backendConfigFile string) (string, error) {
	if backendConfigJSON != "" && backendConfigFile != "" {
		return "", fmt.Errorf("--backend-config conflicts with --backend-config-file")
	}

	if backendConfigFile != "" {
		_backendConfigJSON, err := os.ReadFile(backendConfigFile)
		if err != nil {
			return "", errors.Wrap(err, "parse backend config file")
		}
		backendConfigJSON = string(_backendConfigJSON)
	}

	return backendConfigJSON, nil
}

func getBackendConfig(c *cli.Context, prefix string, required bool) (string, string, error) {
	backendType := c.String(prefix + "backend-type")
	if backendType == "" {
		if required {
			return "", "", errors.Errorf("backend type is empty, please specify option '--%sbackend-type'", prefix)
		}
		return "", "", nil
	}

	possibleBackendTypes := []string{"oss", "s3", "localfs"}
	if !isPossibleValue(possibleBackendTypes, backendType) {
		return "", "", fmt.Errorf("--%sbackend-type should be one of %v", prefix, possibleBackendTypes)
	}

	backendConfig, err := parseBackendConfig(
		c.String(prefix+"backend-config"), c.String(prefix+"backend-config-file"),
	)
	if err != nil {
		return "", "", err
	} else if (backendType == "oss" || backendType == "s3" || backendType == "localfs") && strings.TrimSpace(backendConfig) == "" {
		return "", "", errors.Errorf("backend configuration is empty, please specify option '--%sbackend-config'", prefix)
	}

	return backendType, backendConfig, nil
}

// Add suffix to source image reference as the target
// image reference, like this:
// Source: localhost:5000/nginx:latest
// Target: localhost:5000/nginx:latest-suffix
func addReferenceSuffix(source, suffix string) (string, error) {
	named, err := docker.ParseDockerRef(source)
	if err != nil {
		return "", fmt.Errorf("invalid source image reference: %s", err)
	}
	if _, ok := named.(docker.Digested); ok {
		return "", fmt.Errorf("unsupported digested image reference: %s", named.String())
	}
	named = docker.TagNameOnly(named)
	target := named.String() + suffix
	return target, nil
}

func getTargetReference(c *cli.Context) (string, error) {
	target := c.String("target")
	targetSuffix := c.String("target-suffix")
	if target != "" && targetSuffix != "" {
		return "", fmt.Errorf("--target conflicts with --target-suffix")
	}
	if target == "" && targetSuffix == "" {
		return "", fmt.Errorf("--target or --target-suffix is required")
	}
	var err error
	if targetSuffix != "" {
		target, err = addReferenceSuffix(c.String("source"), targetSuffix)
		if err != nil {
			return "", err
		}
	}
	return target, nil
}

func getCacheReference(c *cli.Context, target string) (string, error) {
	cache := c.String("build-cache")
	cacheTag := c.String("build-cache-tag")
	if cache != "" && cacheTag != "" {
		return "", fmt.Errorf("--build-cache conflicts with --build-cache-tag")
	}
	if cacheTag != "" {
		named, err := docker.ParseDockerRef(target)
		if err != nil {
			return "", fmt.Errorf("invalid target image reference: %s", err)
		}
		cache = fmt.Sprintf("%s/%s:%s", docker.Domain(named), docker.Path(named), cacheTag)
	}
	return cache, nil
}

func getPrefetchPatterns(c *cli.Context) (string, error) {
	prefetchedDir := c.String("prefetch-dir")
	prefetchPatterns := c.Bool("prefetch-patterns")

	if len(prefetchedDir) > 0 && prefetchPatterns {
		return "", fmt.Errorf("--prefetch-dir conflicts with --prefetch-patterns")
	}

	var patterns string

	if prefetchPatterns {
		bytes, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", errors.Wrap(err, "read prefetch patterns from STDIN")
		}
		patterns = string(bytes)
	}

	if len(prefetchedDir) > 0 {
		patterns = prefetchedDir
	}

	if len(patterns) == 0 {
		patterns = "/"
	}

	return patterns, nil
}

func main() {
	logrus.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
	})

	version := fmt.Sprintf("\nVersion	: %s\nRevision	: %s\nGo version	: %s\nBuild time	: %s", gitVersion, revision, runtime.Version(), buildTime)

	app := &cli.App{
		Name:    "Nydusify",
		Usage:   "Nydus utility tool to build, convert, verify and view container images",
		Version: version,
	}

	// global options
	app.Flags = getGlobalFlags()

	app.Commands = []*cli.Command{
		{
			Name:  "convert",
			Usage: "Generate a Nydus image from an OCI image",
			Flags: []cli.Flag{
				&cli.StringFlag{
					Name:     "source",
					Required: true,
					Usage:    "Source OCI image reference",
					EnvVars:  []string{"SOURCE"},
				},
				&cli.StringFlag{
					Name:     "target",
					Required: false,
					Usage:    "Target (Nydus) image reference",
					EnvVars:  []string{"TARGET"},
				},
				&cli.StringFlag{
					Name:    "source-backend-type",
					Value:   "",
					Usage:   "Type of storage backend, possible values: 'oss', 's3'",
					EnvVars: []string{"BACKEND_TYPE"},
				},
				&cli.StringFlag{
					Name:    "source-backend-config",
					Value:   "",
					Usage:   "Json configuration string for storage backend",
					EnvVars: []string{"BACKEND_CONFIG"},
				},
				&cli.StringFlag{
					Name:     "target-suffix",
					Required: false,
					Usage:    "Generate the target image reference by adding a suffix to the source image reference, conflicts with --target",
					EnvVars:  []string{"TARGET_SUFFIX"},
				},
				&cli.BoolFlag{
					Name:     "source-insecure",
					Required: false,
					Usage:    "Skip verifying server certs for HTTPS source registry",
					EnvVars:  []string{"SOURCE_INSECURE"},
				},
				&cli.BoolFlag{
					Name:     "target-insecure",
					Required: false,
					Usage:    "Skip verifying server certs for HTTPS target registry",
					EnvVars:  []string{"TARGET_INSECURE"},
				},

				&cli.StringFlag{
					Name:    "backend-type",
					Value:   "",
					Usage:   "Type of storage backend, possible values: 'oss', 's3'",
					EnvVars: []string{"BACKEND_TYPE"},
				},
				&cli.StringFlag{
					Name:    "backend-config",
					Value:   "",
					Usage:   "Json configuration string for storage backend",
					EnvVars: []string{"BACKEND_CONFIG"},
				},
				&cli.PathFlag{
					Name:      "backend-config-file",
					Value:     "",
					TakesFile: true,
					Usage:     "Json configuration file for storage backend",
					EnvVars:   []string{"BACKEND_CONFIG_FILE"},
				},
				&cli.BoolFlag{
					Name:  "backend-force-push",
					Value: false, Usage: "Force to push Nydus blobs even if they already exist in storage backend",
					EnvVars: []string{"BACKEND_FORCE_PUSH"},
				},

				&cli.StringFlag{
					Name:    "build-cache",
					Value:   "",
					Usage:   "Specify a cache image to accelerate nydus image conversion",
					EnvVars: []string{"BUILD_CACHE"},
				},
				&cli.StringFlag{
					Name:    "build-cache-tag",
					Value:   "",
					Usage:   "Use $target:$build-cache-tag as cache image, conflict with --build-cache",
					EnvVars: []string{"BUILD_CACHE_TAG"},
				},
				&cli.StringFlag{
					Name:    "build-cache-version",
					Value:   "v1",
					Usage:   "Version number to filter cache images",
					EnvVars: []string{"BUILD_CACHE_VERSION"},
				},
				&cli.BoolFlag{
					Name:     "build-cache-insecure",
					Required: false,
					Usage:    "Skip verifying server certs for HTTPS cache registry",
					EnvVars:  []string{"BUILD_CACHE_INSECURE"},
				},
				// The --build-cache-max-records flag represents the maximum number
				// of layers in cache image. 200 (bootstrap + blob in one record) was
				// chosen to make it compatible with the 127 max in graph driver of
				// docker so that we can pull cache image using docker.
				&cli.UintFlag{
					Name:    "build-cache-max-records",
					Value:   maxCacheMaxRecords,
					Usage:   "Maximum cache records in a cache image",
					EnvVars: []string{"BUILD_CACHE_MAX_RECORDS"},
				},
				&cli.StringFlag{
					Name:     "chunk-dict",
					Required: false,
					Usage: "Specify a chunk dict expression for chunk deduplication, " +
						"for examples: bootstrap:registry:localhost:5000/namespace/app:chunk_dict, bootstrap:local:/path/to/chunk_dict.boot",
					EnvVars: []string{"CHUNK_DICT"},
				},
				&cli.BoolFlag{
					Name:     "chunk-dict-insecure",
					Required: false,
					Value:    false,
					Usage:    "Skip verifying server certs for HTTPS dict registry",
					EnvVars:  []string{"CHUNK_DICT_INSECURE"},
				},

				&cli.BoolFlag{
					Name:    "merge-platform",
					Value:   false,
					Usage:   "Generate an OCI image index with both OCI and Nydus manifests for the image",
					EnvVars: []string{"MERGE_PLATFORM"},
					Aliases: []string{"multi-platform"},
				},
				&cli.BoolFlag{
					Name:  "all-platforms",
					Value: false,
					Usage: "Convert images for all platforms, conflicts with --platform",
				},
				&cli.StringFlag{
					Name:  "platform",
					Value: "linux/" + runtime.GOARCH,
					Usage: "Convert images for specific platforms, for example: 'linux/amd64,linux/arm64'",
				},
				&cli.BoolFlag{
					Name:    "oci-ref",
					Value:   false,
					Usage:   "Convert to OCI-referenced nydus zran image",
					EnvVars: []string{"OCI_REF"},
				},
				&cli.BoolFlag{
					Name:    "with-referrer",
					Value:   false,
					Usage:   "Associate a reference to the source image, see https://github.com/opencontainers/distribution-spec/blob/main/spec.md#listing-referrers",
					EnvVars: []string{"WITH_REFERRER"},
				},
				&cli.BoolFlag{
					Name:    "oci",
					Value:   false,
					Usage:   "Convert Docker media types to OCI media types",
					EnvVars: []string{"OCI"},
				},
				&cli.BoolFlag{
					Name:   "docker-v2-format",
					Value:  false,
					Hidden: true,
				},
				&cli.StringFlag{
					Name:        "fs-version",
					Required:    false,
					Value:       "6",
					DefaultText: "V6 nydus image format",
					Usage:       "Nydus image format version number, possible values: 5, 6",
					EnvVars:     []string{"FS_VERSION"},
				},
				&cli.BoolFlag{
					Name:    "fs-align-chunk",
					Value:   false,
					Usage:   "Enable chunk data alignment(4K) for Nydus image",
					EnvVars: []string{"FS_ALIGN_CHUNK"},
				},
				&cli.BoolFlag{
					Name:    "backend-aligned-chunk",
					Value:   false,
					Usage:   "[Deprecated] Enable chunk data alignment(4K) for Nydus image",
					EnvVars: []string{"BACKEND_ALIGNED_CHUNK"},
				},
				&cli.StringFlag{
					Name:    "prefetch-dir",
					Value:   "",
					Usage:   "Specify an absolute path within the image for prefetch",
					EnvVars: []string{"PREFETCH_DIR"},
				},
				&cli.BoolFlag{
					Name:    "prefetch-patterns",
					Value:   false,
					Usage:   "Read prefetch list from STDIN, please input absolute paths line by line",
					EnvVars: []string{"PREFETCH_PATTERNS"},
				},
				&cli.StringFlag{
					Name:    "compressor",
					Value:   "zstd",
					Usage:   "Algorithm to compress image data blob, possible values: none, lz4_block, zstd",
					EnvVars: []string{"COMPRESSOR"},
				},
				&cli.StringFlag{
					Name:    "fs-chunk-size",
					Value:   "0x100000",
					Usage:   "size of nydus image data chunk, must be power of two and between 0x1000-0x10000000, [default: 0x4000000]",
					EnvVars: []string{"FS_CHUNK_SIZE"},
					Aliases: []string{"chunk-size"},
				},
				&cli.StringFlag{
					Name:    "batch-size",
					Value:   "0",
					Usage:   "size of batch data chunks, must be power of two, between 0x1000-0x1000000 or zero, [default: 0]",
					EnvVars: []string{"BATCH_SIZE"},
				},
				&cli.StringFlag{
					Name:    "work-dir",
					Value:   "./tmp",
					Usage:   "Working directory for image conversion",
					EnvVars: []string{"WORK_DIR"},
				},
				&cli.StringFlag{
					Name:    "nydus-image",
					Value:   "nydus-image",
					Usage:   "Path to the nydus-image binary, default to search in PATH",
					EnvVars: []string{"NYDUS_IMAGE"},
				},
				&cli.StringFlag{
					Name:    "output-json",
					Value:   "",
					Usage:   "File path to save the metrics collected during conversion in JSON format, for example: './output.json'",
					EnvVars: []string{"OUTPUT_JSON"},
				},
				&cli.BoolFlag{
					Name:    "plain-http",
					Value:   false,
					Usage:   "Enable plain http for Nydus image push",
					EnvVars: []string{"PLAIN_HTTP"},
				},
				&cli.IntFlag{
					Name:    "push-retry-count",
					Value:   3,
					Usage:   "Number of retries when pushing to registry fails",
					EnvVars: []string{"PUSH_RETRY_COUNT"},
				},
				&cli.StringFlag{
					Name:    "push-retry-delay",
					Value:   "5s",
					Usage:   "Delay between push retries (e.g. 5s, 1m, 1h)",
					EnvVars: []string{"PUSH_RETRY_DELAY"},
				},
			},
			Action: func(c *cli.Context) error {
				setupLogLevel(c)

				targetRef, err := getTargetReference(c)
				if err != nil {
					return err
				}

				backendType, backendConfig, err := getBackendConfig(c, "", false)
				if err != nil {
					return err
				}

				cacheRef, err := getCacheReference(c, targetRef)
				if err != nil {
					return err
				}
				cacheMaxRecords := c.Uint("build-cache-max-records")
				if cacheMaxRecords < 1 {
					return fmt.Errorf("--build-cache-max-records should be greater than 0")
				}
				if cacheMaxRecords > maxCacheMaxRecords {
					return fmt.Errorf("--build-cache-max-records should not be greater than %d", maxCacheMaxRecords)
				}
				cacheVersion := c.String("build-cache-version")

				fsVersion := c.String("fs-version")
				possibleFsVersions := []string{"5", "6"}
				if !isPossibleValue(possibleFsVersions, fsVersion) {
					return fmt.Errorf("--fs-version should be one of %v", possibleFsVersions)
				}

				prefetchPatterns, err := getPrefetchPatterns(c)
				if err != nil {
					return err
				}

				chunkDictRef := ""
				chunkDict := c.String("chunk-dict")
				if chunkDict != "" {
					_, _, chunkDictRef, err = converter.ParseChunkDictArgs(chunkDict)
					if err != nil {
						return errors.Wrap(err, "parse chunk dict arguments")
					}
				}

				docker2OCI := false
				if c.Bool("docker-v2-format") {
					logrus.Warn("the option `--docker-v2-format` has been deprecated, use `--oci` instead")
					docker2OCI = false
				} else if c.Bool("oci") {
					docker2OCI = true
				}

				// Forcibly enable `--oci` option when `--oci-ref` be enabled.
				if c.Bool("oci-ref") {
					logrus.Warn("forcibly enabled `--oci` option when `--oci-ref` be enabled")
					docker2OCI = true
				}

				opt := converter.Opt{
					WorkDir:        c.String("work-dir"),
					NydusImagePath: c.String("nydus-image"),

					SourceBackendType:   c.String("source-backend-type"),
					SourceBackendConfig: c.String("source-backend-config"),
					Source:              c.String("source"),
					Target:              targetRef,
					SourceInsecure:      c.Bool("source-insecure"),
					TargetInsecure:      c.Bool("target-insecure"),

					BackendType:      backendType,
					BackendConfig:    backendConfig,
					BackendForcePush: c.Bool("backend-force-push"),

					CacheRef:        cacheRef,
					CacheInsecure:   c.Bool("build-cache-insecure"),
					CacheMaxRecords: cacheMaxRecords,
					CacheVersion:    cacheVersion,

					ChunkDictRef:      chunkDictRef,
					ChunkDictInsecure: c.Bool("chunk-dict-insecure"),

					PrefetchPatterns: prefetchPatterns,
					MergePlatform:    c.Bool("merge-platform"),
					Docker2OCI:       docker2OCI,
					FsVersion:        fsVersion,
					FsAlignChunk:     c.Bool("backend-aligned-chunk") || c.Bool("fs-align-chunk"),
					Compressor:       c.String("compressor"),
					ChunkSize:        c.String("chunk-size"),
					BatchSize:        c.String("batch-size"),

					OCIRef:       c.Bool("oci-ref"),
					WithReferrer: c.Bool("with-referrer"),
					AllPlatforms: c.Bool("all-platforms"),
					Platforms:    c.String("platform"),

					OutputJSON:     c.String("output-json"),
					WithPlainHTTP:  c.Bool("plain-http"),
					PushRetryCount: c.Int("push-retry-count"),
					PushRetryDelay: c.String("push-retry-delay"),
				}

				return converter.Convert(context.Background(), opt)
			},
		},
		{
			Name:  "check",
			Usage: "Verify nydus image format and content",
			Flags: []cli.Flag{
				&cli.StringFlag{
					Name:     "source",
					Required: false,
					Usage:    "Source OCI image reference",
					EnvVars:  []string{"SOURCE"},
				},
				&cli.StringFlag{
					Name:     "target",
					Required: true,
					Usage:    "Target (Nydus) image reference",
					EnvVars:  []string{"TARGET"},
				},
				&cli.BoolFlag{
					Name:     "source-insecure",
					Required: false,
					Usage:    "Skip verifying server certs for HTTPS source registry",
					EnvVars:  []string{"SOURCE_INSECURE"},
				},
				&cli.BoolFlag{
					Name:     "target-insecure",
					Required: false,
					Usage:    "Skip verifying server certs for HTTPS target registry",
					EnvVars:  []string{"TARGET_INSECURE"},
				},

				&cli.StringFlag{
					Name:    "source-backend-type",
					Value:   "",
					Usage:   "Type of storage backend, possible values: 'oss', 's3'",
					EnvVars: []string{"BACKEND_TYPE"},
				},
				&cli.StringFlag{
					Name:    "source-backend-config",
					Value:   "",
					Usage:   "Json configuration string for storage backend",
					EnvVars: []string{"BACKEND_CONFIG"},
				},
				&cli.PathFlag{
					Name:      "source-backend-config-file",
					Value:     "",
					TakesFile: true,
					Usage:     "Json configuration file for storage backend",
					EnvVars:   []string{"BACKEND_CONFIG_FILE"},
				},

				&cli.StringFlag{
					Name:    "target-backend-type",
					Value:   "",
					Usage:   "Type of storage backend, possible values: 'oss', 's3'",
					EnvVars: []string{"BACKEND_TYPE"},
				},
				&cli.StringFlag{
					Name:    "target-backend-config",
					Value:   "",
					Usage:   "Json configuration string for storage backend",
					EnvVars: []string{"BACKEND_CONFIG"},
				},
				&cli.PathFlag{
					Name:      "target-backend-config-file",
					Value:     "",
					TakesFile: true,
					Usage:     "Json configuration file for storage backend",
					EnvVars:   []string{"BACKEND_CONFIG_FILE"},
				},

				&cli.BoolFlag{
					Name:    "multi-platform",
					Value:   false,
					Usage:   "Verify that the image contains an image index with both OCI and Nydus manifests",
					EnvVars: []string{"MULTI_PLATFORM"},
				},
				&cli.StringFlag{
					Name:  "platform",
					Value: "linux/" + runtime.GOARCH,
					Usage: "Specify platform identifier to choose image manifest, possible values: 'linux/amd64' and 'linux/arm64'",
				},

				&cli.StringFlag{
					Name:    "work-dir",
					Value:   "./output",
					Usage:   "Working directory for image verification",
					EnvVars: []string{"WORK_DIR"},
				},
				&cli.StringFlag{
					Name:    "nydus-image",
					Value:   "nydus-image",
					Usage:   "Path to the nydus-image binary, default to search in PATH",
					EnvVars: []string{"NYDUS_IMAGE"},
				},
				&cli.StringFlag{
					Name:    "nydusd",
					Value:   "nydusd",
					Usage:   "Path to the nydusd binary, default to search in PATH",
					EnvVars: []string{"NYDUSD"},
				},
			},
			Action: func(c *cli.Context) error {
				setupLogLevel(c)

				sourceBackendType, sourceBackendConfig, err := getBackendConfig(c, "source-", false)
				if err != nil {
					return err
				}

				targetBackendType, targetBackendConfig, err := getBackendConfig(c, "target-", false)
				if err != nil {
					return err
				}

				_, arch, err := provider.ExtractOsArch(c.String("platform"))
				if err != nil {
					return err
				}

				checker, err := checker.New(checker.Opt{
					WorkDir: c.String("work-dir"),

					Source:              c.String("source"),
					Target:              c.String("target"),
					SourceInsecure:      c.Bool("source-insecure"),
					TargetInsecure:      c.Bool("target-insecure"),
					SourceBackendType:   sourceBackendType,
					SourceBackendConfig: sourceBackendConfig,
					TargetBackendType:   targetBackendType,
					TargetBackendConfig: targetBackendConfig,

					MultiPlatform:  c.Bool("multi-platform"),
					NydusImagePath: c.String("nydus-image"),
					NydusdPath:     c.String("nydusd"),
					ExpectedArch:   arch,
				})
				if err != nil {
					return err
				}

				return checker.Check(context.Background())
			},
		},
		{
			Name:  "chunkdict",
			Usage: "Deduplicate chunk for Nydus image (experimental)",
			Subcommands: []*cli.Command{
				{
					Name:  "generate",
					Usage: "Save chunk and blob information of Multi-image into the database (experimental)",
					Flags: []cli.Flag{
						&cli.StringSliceFlag{
							Name:     "sources",
							Required: true,
							Usage:    "One or more Nydus image reference(Multiple images should be split by commas)",
							EnvVars:  []string{"SOURCES"},
						},
						&cli.StringFlag{
							Name:     "target",
							Required: false,
							Usage:    "Target chunkdict image (Nydus) reference",
							EnvVars:  []string{"TARGET"},
						},
						&cli.BoolFlag{
							Name:     "source-insecure",
							Required: false,
							Usage:    "Skip verifying server certs for HTTPS source registry",
							EnvVars:  []string{"SOURCE_INSECURE"},
						},
						&cli.BoolFlag{
							Name:     "target-insecure",
							Required: false,
							Usage:    "Skip verifying server certs for HTTPS target registry",
							EnvVars:  []string{"TARGET_INSECURE"},
						},

						&cli.StringFlag{
							Name:    "backend-type",
							Value:   "",
							Usage:   "Type of storage backend, possible values: 'oss', 's3'",
							EnvVars: []string{"BACKEND_TYPE"},
						},
						&cli.StringFlag{
							Name:    "backend-config",
							Value:   "",
							Usage:   "Json configuration string for storage backend",
							EnvVars: []string{"BACKEND_CONFIG"},
						},
						&cli.PathFlag{
							Name:      "backend-config-file",
							Value:     "",
							TakesFile: true,
							Usage:     "Json configuration file for storage backend",
							EnvVars:   []string{"BACKEND_CONFIG_FILE"},
						},

						&cli.StringFlag{
							Name:    "work-dir",
							Value:   "./output",
							Usage:   "Working directory for generating chunkdict image",
							EnvVars: []string{"WORK_DIR"},
						},
						&cli.StringFlag{
							Name:    "nydus-image",
							Value:   "nydus-image",
							Usage:   "Path to the nydus-image binary, default to search in PATH",
							EnvVars: []string{"NYDUS_IMAGE"},
						},

						&cli.BoolFlag{
							Name:  "all-platforms",
							Value: false,
							Usage: "Generate chunkdict image for all platforms, conflicts with --platform",
						},
						&cli.StringFlag{
							Name:  "platform",
							Value: "linux/" + runtime.GOARCH,
							Usage: "Specify platform identifier to choose image manifest, possible values: 'linux/amd64' and 'linux/arm64'",
						},
					},
					Action: func(c *cli.Context) error {
						setupLogLevel(c)

						backendType, backendConfig, err := getBackendConfig(c, "", false)
						if err != nil {
							return err
						}

						_, arch, err := provider.ExtractOsArch(c.String("platform"))
						if err != nil {
							return err
						}

						generator, err := generator.New(generator.Opt{
							Sources:        c.StringSlice("sources"),
							Target:         c.String("target"),
							SourceInsecure: c.Bool("source-insecure"),
							TargetInsecure: c.Bool("target-insecure"),

							BackendType:      backendType,
							BackendConfig:    backendConfig,
							BackendForcePush: c.Bool("backend-force-push"),

							WorkDir:        c.String("work-dir"),
							NydusImagePath: c.String("nydus-image"),
							ExpectedArch:   arch,
							AllPlatforms:   c.Bool("all-platforms"),
							Platforms:      c.String("platform"),
						})
						if err != nil {
							return err
						}

						return generator.Generate(context.Background())
					},
				},
			},
		},
		{
			Name:    "mount",
			Aliases: []string{"view"},
			Usage:   "Mount the nydus image as a filesystem",
			Flags: []cli.Flag{
				&cli.StringFlag{
					Name:     "target",
					Required: true,
					Usage:    "Target (Nydus) image reference",
					EnvVars:  []string{"TARGET"},
				},
				&cli.BoolFlag{
					Name:     "target-insecure",
					Required: false,
					Usage:    "Skip verifying server certs for HTTPS target registry",
					EnvVars:  []string{"TARGET_INSECURE"},
				},

				&cli.StringFlag{
					Name:     "backend-type",
					Value:    "",
					Required: false,
					Usage:    "Type of storage backend, possible values: 'oss', 's3'",
					EnvVars:  []string{"BACKEND_TYPE"},
				},
				&cli.StringFlag{
					Name:    "backend-config",
					Value:   "",
					Usage:   "Json configuration string for storage backend",
					EnvVars: []string{"BACKEND_CONFIG"},
				},
				&cli.PathFlag{
					Name:      "backend-config-file",
					Value:     "",
					TakesFile: true,
					Usage:     "Json configuration file for storage backend",
					EnvVars:   []string{"BACKEND_CONFIG_FILE"},
				},
				&cli.BoolFlag{
					Name:    "prefetch",
					Value:   false,
					Usage:   "Enable full image data prefetch",
					EnvVars: []string{"PREFETCH"},
				},
				&cli.StringFlag{
					Name:    "mount-path",
					Value:   "./image-fs",
					Usage:   "Path to mount the image",
					EnvVars: []string{"MOUNT_PATH"},
				},
				&cli.StringFlag{
					Name:  "platform",
					Value: "linux/" + runtime.GOARCH,
					Usage: "Specify platform identifier to choose image manifest, possible values: 'linux/amd64' and 'linux/arm64'",
				},

				&cli.StringFlag{
					Name:    "work-dir",
					Value:   "./tmp",
					Usage:   "Working directory for image view, will be cleaned up after viewing",
					EnvVars: []string{"WORK_DIR"},
				},
				&cli.StringFlag{
					Name:    "nydusd",
					Value:   "nydusd",
					Usage:   "The nydusd binary path, if unset, search in PATH environment",
					EnvVars: []string{"NYDUSD"},
				},
			},
			Action: func(c *cli.Context) error {
				setupLogLevel(c)

				backendType, backendConfig, err := getBackendConfig(c, "", false)
				if err != nil {
					return err
				} else if backendConfig == "" {

					backendType = "registry"
					parsed, err := reference.ParseNormalizedNamed(c.String("target"))
					if err != nil {
						return err
					}

					backendConfigStruct, err := utils.NewRegistryBackendConfig(parsed, c.Bool("target-insecure"))
					if err != nil {
						return errors.Wrap(err, "parse registry backend configuration")
					}

					bytes, err := json.Marshal(backendConfigStruct)
					if err != nil {
						return errors.Wrap(err, "marshal registry backend configuration")
					}
					backendConfig = string(bytes)

				}

				_, arch, err := provider.ExtractOsArch(c.String("platform"))
				if err != nil {
					return err
				}

				fsViewer, err := viewer.New(viewer.Opt{
					WorkDir:        c.String("work-dir"),
					Target:         c.String("target"),
					TargetInsecure: c.Bool("target-insecure"),
					MountPath:      c.String("mount-path"),
					NydusdPath:     c.String("nydusd"),
					BackendType:    backendType,
					BackendConfig:  backendConfig,
					ExpectedArch:   arch,
					Prefetch:       c.Bool("prefetch"),
				})
				if err != nil {
					return err
				}

				return fsViewer.View(context.Background())
			},
		},
		{
			Name:    "build",
			Aliases: []string{"pack"},
			Usage:   "Build a Nydus filesystem from a source directory",
			Flags: []cli.Flag{
				&cli.StringFlag{
					Name:     "source-dir",
					Aliases:  []string{"target-dir"}, // for compatibility
					Required: true,
					Usage:    "Source directory to build Nydus filesystem from",
					EnvVars:  []string{"SOURCE_DIR"},
				},
				&cli.StringFlag{
					Name:     "output-dir",
					Aliases:  []string{"o"},
					Required: false,
					Usage:    "Output directory for built artifacts",
					EnvVars:  []string{"OUTPUT_DIR"},
				},
				&cli.StringFlag{
					Name:     "name",
					Aliases:  []string{"meta", "bootstrap"}, // for compatibility
					Required: true,
					Usage:    "Image name, which will be used as suffix for the generated Nydus image bootstrap/data blobs",
					EnvVars:  []string{"BOOTSTRAP", "IMAGE_NAME"},
				},

				&cli.BoolFlag{
					Name:    "backend-push",
					Value:   false,
					Usage:   "Push generated Nydus filesystem to storage backend",
					EnvVars: []string{"BACKEND_PUSH"},
				},
				&cli.StringFlag{
					Name:        "backend-type",
					Value:       "oss",
					DefaultText: "oss",
					Usage:       "Type of storage backend, possible values: 'oss', 's3'",
					EnvVars:     []string{"BACKEND_TYPE"},
				},
				&cli.StringFlag{
					Name:    "backend-config",
					Value:   "",
					Usage:   "Json configuration string for storage backend",
					EnvVars: []string{"BACKEND_CONFIG"},
				},
				&cli.PathFlag{
					Name:      "backend-config-file",
					TakesFile: true,
					Usage:     "Json configuration file for storage backend",
					EnvVars:   []string{"BACKEND_CONFIG_FILE"},
				},

				&cli.StringFlag{
					Name:    "chunk-dict",
					Usage:   "Specify a chunk dict expression for chunk deduplication, for example: bootstrap=/path/to/dict.boot",
					EnvVars: []string{"CHUNK_DICT"},
				},
				&cli.StringFlag{
					Name:    "parent-bootstrap",
					Usage:   "Specify a parent metadata to reference data chunks",
					EnvVars: []string{"PARENT_BOOTSTRAP"},
				},
				&cli.BoolFlag{
					Name:    "compact",
					Usage:   "Compact parent bootstrap before building the image when needed",
					EnvVars: []string{"COMPACT"},
				},
				&cli.PathFlag{
					Name:      "compact-config-file",
					TakesFile: true,
					Usage: "Compact configuration file, default configuration is " +
						"{\"min_used_ratio\": 5, \"compact_blob_size\": 10485760, \"max_compact_size\": 104857600, " +
						"\"layers_to_compact\": 32}",
					EnvVars: []string{"COMPACT_CONFIG_FILE"},
				},

				&cli.StringFlag{
					Name:        "fs-version",
					Required:    false,
					Usage:       "Nydus image format version number, possible values: 5, 6",
					EnvVars:     []string{"FS_VERSION"},
					Value:       "6",
					DefaultText: "V6 nydus image format",
				},
				&cli.StringFlag{
					Name:    "compressor",
					Value:   "zstd",
					Usage:   "Algorithm to compress image data blob, possible values: none, lz4_block, zstd",
					EnvVars: []string{"COMPRESSOR"},
				},
				&cli.StringFlag{
					Name:    "chunk-size",
					Value:   "0x100000",
					Usage:   "size of nydus image data chunk, must be power of two and between 0x1000-0x100000, [default: 0x100000]",
					EnvVars: []string{"CHUNK_SIZE"},
				},

				&cli.StringFlag{
					Name:    "nydus-image",
					Value:   "nydus-image",
					Usage:   "Path to the nydus-image binary, default to search in PATH",
					EnvVars: []string{"NYDUS_IMAGE"},
				},
			},
			Before: func(ctx *cli.Context) error {
				sourcePath := ctx.String("source-dir")
				fi, err := os.Stat(sourcePath)
				if err != nil {
					return errors.Wrapf(err, "failed to check source directory")
				}
				if !fi.IsDir() {
					return errors.Errorf("source path '%s' is not a directory", sourcePath)
				}
				return nil
			},
			Action: func(c *cli.Context) error {
				setupLogLevel(c)

				var (
					p             *packer.Packer
					res           packer.PackResult
					backendConfig packer.BackendConfig
					err           error
				)

				// if backend-push is specified, we should make sure backend-config-file exists
				if c.Bool("backend-push") || c.Bool("compact") {
					_backendType, _backendConfig, err := getBackendConfig(c, "", true)
					if err != nil {
						return err
					}
					// we can verify the _backendType in the `packer.ParseBackendConfigString` function
					cfg, err := packer.ParseBackendConfigString(_backendType, _backendConfig)
					if err != nil {
						return errors.Errorf("failed to parse backend-config '%s', err = %v", _backendConfig, err)
					}
					backendConfig = cfg
				}

				if p, err = packer.New(packer.Opt{
					LogLevel:       logrus.GetLevel(),
					NydusImagePath: c.String("nydus-image"),
					OutputDir:      c.String("output-dir"),
					BackendConfig:  backendConfig,
				}); err != nil {
					return err
				}

				if res, err = p.Pack(context.Background(), packer.PackRequest{
					SourceDir:    c.String("source-dir"),
					ImageName:    c.String("name"),
					PushToRemote: c.Bool("backend-push"),
					FsVersion:    c.String("fs-version"),
					Compressor:   c.String("compressor"),
					ChunkSize:    c.String("chunk-size"),

					ChunkDict:         c.String("chunk-dict"),
					Parent:            c.String("parent-bootstrap"),
					TryCompact:        c.Bool("compact"),
					CompactConfigPath: c.String("compact-config-file"),
				}); err != nil {
					return err
				}
				logrus.Infof("successfully built Nydus image (bootstrap:'%s', blob:'%s')", res.Meta, res.Blob)
				return nil
			},
		},
		{
			Name:  "copy",
			Usage: "Copy an image from source to target",
			Flags: []cli.Flag{
				&cli.StringFlag{
					Name:     "source",
					Required: true,
					Usage:    "Source image reference",
					EnvVars:  []string{"SOURCE"},
				},
				&cli.StringFlag{
					Name:     "target",
					Required: false,
					Usage:    "Target image reference",
					EnvVars:  []string{"TARGET"},
				},
				&cli.BoolFlag{
					Name:     "source-insecure",
					Required: false,
					Usage:    "Skip verifying server certs for HTTPS source registry",
					EnvVars:  []string{"SOURCE_INSECURE"},
				},
				&cli.BoolFlag{
					Name:     "target-insecure",
					Required: false,
					Usage:    "Skip verifying server certs for HTTPS target registry",
					EnvVars:  []string{"TARGET_INSECURE"},
				},

				&cli.StringFlag{
					Name:    "source-backend-type",
					Value:   "",
					Usage:   "Type of storage backend, possible values: 'oss', 's3'",
					EnvVars: []string{"BACKEND_TYPE"},
				},
				&cli.StringFlag{
					Name:    "source-backend-config",
					Value:   "",
					Usage:   "Json configuration string for storage backend",
					EnvVars: []string{"BACKEND_CONFIG"},
				},
				&cli.PathFlag{
					Name:      "source-backend-config-file",
					Value:     "",
					TakesFile: true,
					Usage:     "Json configuration file for storage backend",
					EnvVars:   []string{"BACKEND_CONFIG_FILE"},
				},

				&cli.BoolFlag{
					Name:  "all-platforms",
					Value: false,
					Usage: "Copy images for all platforms, conflicts with --platform",
				},
				&cli.StringFlag{
					Name:  "platform",
					Value: "linux/" + runtime.GOARCH,
					Usage: "Copy images for specific platforms, for example: 'linux/amd64,linux/arm64'",
				},

				&cli.StringFlag{
					Name:  "push-chunk-size",
					Value: "0MB",
					Usage: "Chunk size for pushing a blob layer in chunked",
				},

				&cli.StringFlag{
					Name:    "work-dir",
					Value:   "./tmp",
					Usage:   "Working directory for image copy",
					EnvVars: []string{"WORK_DIR"},
				},
				&cli.StringFlag{
					Name:    "nydus-image",
					Value:   "nydus-image",
					Usage:   "Path to the nydus-image binary, default to search in PATH",
					EnvVars: []string{"NYDUS_IMAGE"},
				},
			},
			Action: func(c *cli.Context) error {
				setupLogLevel(c)

				sourceBackendType, sourceBackendConfig, err := getBackendConfig(c, "source-", false)
				if err != nil {
					return err
				}

				pushChunkSize, err := humanize.ParseBytes(c.String("push-chunk-size"))
				if err != nil {
					return errors.Wrap(err, "invalid --push-chunk-size option")
				}
				if pushChunkSize > 0 {
					logrus.Infof("will copy layer with chunk size %s", c.String("push-chunk-size"))
				}

				opt := copier.Opt{
					WorkDir:        c.String("work-dir"),
					NydusImagePath: c.String("nydus-image"),

					Source:         c.String("source"),
					Target:         c.String("target"),
					SourceInsecure: c.Bool("source-insecure"),
					TargetInsecure: c.Bool("target-insecure"),

					SourceBackendType:   sourceBackendType,
					SourceBackendConfig: sourceBackendConfig,

					AllPlatforms: c.Bool("all-platforms"),
					Platforms:    c.String("platform"),

					PushChunkSize: int64(pushChunkSize),
				}

				return copier.Copy(context.Background(), opt)
			},
		},
		{
			Name:  "optimize",
			Usage: "Optimize a source nydus image and push to the target",
			Flags: []cli.Flag{
				&cli.StringFlag{
					Name:     "source",
					Required: true,
					Usage:    "Source (Nydus) image reference",
					EnvVars:  []string{"SOURCE"},
				},
				&cli.StringFlag{
					Name:     "target",
					Required: true,
					Usage:    "Target (Nydus) image reference",
					EnvVars:  []string{"TARGET"},
				},
				&cli.BoolFlag{
					Name:     "source-insecure",
					Required: false,
					Usage:    "Skip verifying server certs for HTTPS source registry",
					EnvVars:  []string{"SOURCE_INSECURE"},
				},
				&cli.BoolFlag{
					Name:     "target-insecure",
					Required: false,
					Usage:    "Skip verifying server certs for HTTPS target registry",
					EnvVars:  []string{"TARGET_INSECURE"},
				},

				&cli.StringFlag{
					Name:    "policy",
					Value:   "separated-blob-with-prefetch-files",
					Usage:   "Specify the optimizing way",
					EnvVars: []string{"OPTIMIZE_POLICY"},
				},
				&cli.StringFlag{
					Name:     "prefetch-files",
					Required: false,
					Usage:    "File path to include prefetch files for optimization",
					EnvVars:  []string{"PREFETCH_FILES"},
				},

				&cli.StringFlag{
					Name:    "work-dir",
					Value:   "./tmp",
					Usage:   "Working directory for image optimization",
					EnvVars: []string{"WORK_DIR"},
				},

				&cli.StringFlag{
					Name:    "nydus-image",
					Value:   "nydus-image",
					Usage:   "Path to the nydus-image binary, default to search in PATH",
					EnvVars: []string{"NYDUS_IMAGE"},
				},

				&cli.StringFlag{
					Name:  "push-chunk-size",
					Value: "0MB",
					Usage: "Chunk size for pushing a blob layer in chunked",
				},
			},
			Action: func(c *cli.Context) error {
				setupLogLevel(c)

				pushChunkSize, err := humanize.ParseBytes(c.String("push-chunk-size"))
				if err != nil {
					return errors.Wrap(err, "invalid --push-chunk-size option")
				}
				if pushChunkSize > 0 {
					logrus.Infof("will push layer with chunk size %s", c.String("push-chunk-size"))
				}
				opt := optimizer.Opt{
					WorkDir:        c.String("work-dir"),
					NydusImagePath: c.String("nydus-image"),

					Source:         c.String("source"),
					Target:         c.String("target"),
					SourceInsecure: c.Bool("source-insecure"),
					TargetInsecure: c.Bool("target-insecure"),

					AllPlatforms: c.Bool("all-platforms"),
					Platforms:    c.String("platform"),

					PushChunkSize:     int64(pushChunkSize),
					PrefetchFilesPath: c.String("prefetch-files"),
				}

				return optimizer.Optimize(context.Background(), opt)
			},
		},
		{
			Name:  "commit",
			Usage: "Create and push a new nydus image from a container's changes that use a nydus image",
			Flags: []cli.Flag{
				&cli.StringFlag{
					Name:    "work-dir",
					Value:   "./tmp",
					Usage:   "Working directory for commit workflow",
					EnvVars: []string{"WORK_DIR"},
				},
				&cli.StringFlag{
					Name:    "nydus-image",
					Value:   "nydus-image",
					Usage:   "Path to the nydus-image binary, default to search in PATH",
					EnvVars: []string{"NYDUS_IMAGE"},
				},
				&cli.StringFlag{
					Name:    "containerd-address",
					Value:   "/run/containerd/containerd.sock",
					Usage:   "Containerd address, optionally with \"unix://\" prefix [$CONTAINERD_ADDRESS] (default \"/run/containerd/containerd.sock\")",
					EnvVars: []string{"CONTAINERD_ADDR"},
				},
				&cli.StringFlag{
					Name:    "namespace",
					Aliases: []string{"n"},
					Value:   "default",
					Usage:   "Container namespace, default with \"default\" namespace",
					EnvVars: []string{"NAMESPACE"},
				},
				&cli.StringFlag{
					Name:     "container",
					Required: true,
					Usage:    "Target container ID (supports short ID, full ID)",
					EnvVars:  []string{"CONTAINER"},
				},
				&cli.StringFlag{
					Name:     "target",
					Required: true,
					Usage:    "Target nydus image reference",
					EnvVars:  []string{"TARGET"},
				},
				&cli.BoolFlag{
					Name:     "source-insecure",
					Required: false,
					Usage:    "Skip verifying server certs for HTTPS source registry",
					EnvVars:  []string{"SOURCE_INSECURE"},
				},
				&cli.BoolFlag{
					Name:     "target-insecure",
					Required: false,
					Usage:    "Skip verifying server certs for HTTPS target registry",
					EnvVars:  []string{"TARGET_INSECURE"},
				},
				&cli.IntFlag{
					Name:        "maximum-times",
					Required:    false,
					DefaultText: "400",
					Value:       400,
					Usage:       "The maximum times allowed to be committed",
					EnvVars:     []string{"MAXIMUM_TIMES"},
				},
				&cli.StringSliceFlag{
					Name:     "with-path",
					Aliases:  []string{"with-mount-path"},
					Required: false,
					Usage:    "The external directory (for example mountpoint) in container that need to be committed",
					EnvVars:  []string{"WITH_PATH"},
				},
			},
			Action: func(c *cli.Context) error {
				setupLogLevel(c)
				parsePaths := func(paths []string) ([]string, []string) {
					withPaths := []string{}
					withoutPaths := []string{}

					for _, path := range paths {
						path = strings.TrimSpace(path)
						if strings.HasPrefix(path, "!") {
							path = strings.TrimLeft(path, "!")
							path = strings.TrimRight(path, "/")
							withoutPaths = append(withoutPaths, path)
						} else {
							withPaths = append(withPaths, path)
						}
					}

					return withPaths, withoutPaths
				}

				withPaths, withoutPaths := parsePaths(c.StringSlice("with-path"))
				opt := committer.Opt{
					WorkDir:           c.String("work-dir"),
					NydusImagePath:    c.String("nydus-image"),
					ContainerdAddress: c.String("containerd-address"),
					Namespace:         c.String("namespace"),
					ContainerID:       c.String("container"),
					TargetRef:         c.String("target"),
					SourceInsecure:    c.Bool("source-insecure"),
					TargetInsecure:    c.Bool("target-insecure"),
					MaximumTimes:      c.Int("maximum-times"),
					WithPaths:         withPaths,
					WithoutPaths:      withoutPaths,
				}
				cm, err := committer.NewCommitter(opt)
				if err != nil {
					return errors.Wrap(err, "failed to create committer instance")
				}
				return cm.Commit(c.Context, opt)
			},
		},
	}

	if !utils.IsSupportedArch(runtime.GOARCH) {
		logrus.Fatal("Nydusify can only work under architecture 'amd64' and 'arm64'")
	}

	if err := app.Run(os.Args); err != nil {
		logrus.Fatal(err)
	}
}

func setupLogLevel(c *cli.Context) {
	// global `-D` has the highest priority
	if c.Bool("D") {
		logrus.SetLevel(logrus.DebugLevel)
		return
	}

	lvl := c.String("log-level")
	logLevel, err := logrus.ParseLevel(lvl)
	if err != nil {
		logrus.Warnf("failed to parse log level(%s): %+v\ndefault log level(info) will be used", lvl, err)
		logLevel = defaultLogLevel
	}

	logrus.SetLevel(logLevel)

	if c.String("log-file") != "" {
		f, err := os.OpenFile(c.String("log-file"), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
		if err != nil {
			logrus.Errorf("failed to open log file: %+v", err)
			return
		}
		logrus.SetOutput(f)
	}
}

func getGlobalFlags() []cli.Flag {
	return []cli.Flag{
		&cli.BoolFlag{
			Name:     "debug",
			Aliases:  []string{"D"},
			Required: false,
			Value:    false,
			Usage:    "Enable debug log level, overwrites the 'log-level' option",
			EnvVars:  []string{"DEBUG_LOG_LEVEL"},
		},
		&cli.StringFlag{
			Name:    "log-level",
			Aliases: []string{"l"},
			Value:   "info",
			Usage:   "Set log level (panic, fatal, error, warn, info, debug, trace)",
			EnvVars: []string{"LOG_LEVEL"},
		},
		&cli.StringFlag{
			Name:     "log-file",
			Required: false,
			Usage:    "Write logs to a file",
			EnvVars:  []string{"LOG_FILE"},
		},
	}
}
