package deploy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"

	"github.com/andybalholm/brotli"
	"github.com/cenkalti/backoff/v4"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-units"
	"github.com/go-errors/errors"
	"github.com/spf13/afero"
	"github.com/spf13/viper"
	"github.com/supabase/cli/internal/db/start"
	"github.com/supabase/cli/internal/utils"
	"github.com/supabase/cli/pkg/api"
)

const (
	eszipContentType       = "application/vnd.denoland.eszip"
	compressedEszipMagicId = "EZBR"
)

func Run(ctx context.Context, slugs []string, projectRef string, noVerifyJWT *bool, importMapPath string, fsys afero.Fs) error {
	// Load function config and project id
	if err := utils.LoadConfigFS(fsys); err != nil {
		return err
	}
	if len(slugs) == 0 {
		allSlugs, err := GetFunctionSlugs(fsys)
		if err != nil {
			return err
		}
		slugs = allSlugs
	} else {
		for _, slug := range slugs {
			if err := utils.ValidateFunctionSlug(slug); err != nil {
				return err
			}
		}
	}
	if len(slugs) == 0 {
		return errors.New("No Functions specified or found in " + utils.Bold(utils.FunctionsDir))
	}
	return deployAll(ctx, slugs, projectRef, importMapPath, noVerifyJWT, fsys)
}

func RunDefault(ctx context.Context, projectRef string, fsys afero.Fs) error {
	slugs, err := GetFunctionSlugs(fsys)
	if len(slugs) == 0 {
		return err
	}
	return deployAll(ctx, slugs, projectRef, "", nil, fsys)
}

func GetFunctionSlugs(fsys afero.Fs) ([]string, error) {
	pattern := filepath.Join(utils.FunctionsDir, "*", "index.ts")
	paths, err := afero.Glob(fsys, pattern)
	if err != nil {
		return nil, errors.Errorf("failed to glob function slugs: %w", err)
	}
	var slugs []string
	for _, path := range paths {
		slug := filepath.Base(filepath.Dir(path))
		if utils.FuncSlugPattern.MatchString(slug) {
			slugs = append(slugs, slug)
		}
	}
	return slugs, nil
}

type eszipFunction struct {
	compressedBody *bytes.Buffer
	entrypointPath string
	importMapPath  string
}

func bundleFunction(ctx context.Context, slug, hostImportMapPath string, fsys afero.Fs) (*eszipFunction, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, errors.Errorf("failed to get working directory: %w", err)
	}

	// Create temp directory to store generated eszip
	hostOutputDir := filepath.Join(utils.TempDir, fmt.Sprintf(".output_%s", slug))
	// BitBucket pipelines require docker bind mounts to be world writable
	if err := fsys.MkdirAll(hostOutputDir, 0777); err != nil {
		return nil, errors.Errorf("failed to mkdir: %w", err)
	}
	defer func() {
		if err := fsys.RemoveAll(hostOutputDir); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}()

	hostFuncDir := filepath.Join(cwd, utils.FunctionsDir)
	dockerFuncDir := utils.ToDockerPath(hostFuncDir)

	outputPath := utils.DockerEszipDir + "/output.eszip"
	binds := []string{
		// Reuse deno cache directory, ie. DENO_DIR, between container restarts
		// https://denolib.gitbook.io/guide/advanced/deno_dir-code-fetch-and-cache
		utils.EdgeRuntimeId + ":/root/.cache/deno:rw",
		hostFuncDir + ":" + dockerFuncDir + ":ro",
		filepath.Join(cwd, hostOutputDir) + ":" + utils.DockerEszipDir + ":rw",
	}

	result := eszipFunction{
		entrypointPath: path.Join(dockerFuncDir, slug, "index.ts"),
		importMapPath:  path.Join(dockerFuncDir, "import_map.json"),
	}
	cmd := []string{"bundle", "--entrypoint", result.entrypointPath, "--output", outputPath}
	if viper.GetBool("DEBUG") {
		cmd = append(cmd, "--verbose")
	}

	if hostImportMapPath != "" {
		modules, dockerImportMapPath, err := utils.BindImportMap(hostImportMapPath, fsys)
		if err != nil {
			return nil, err
		}
		binds = append(binds, modules...)
		result.importMapPath = dockerImportMapPath
		cmd = append(cmd, "--import-map", result.importMapPath)
	}

	err = utils.DockerRunOnceWithConfig(
		ctx,
		container.Config{
			Image: utils.EdgeRuntimeImage,
			Env:   []string{},
			Cmd:   cmd,
		},
		start.WithSyslogConfig(container.HostConfig{
			Binds: binds,
		}),
		network.NetworkingConfig{},
		"",
		os.Stdout,
		os.Stderr,
	)
	if err != nil {
		return nil, err
	}

	eszipBytes, err := fsys.Open(filepath.Join(hostOutputDir, "output.eszip"))
	if err != nil {
		return nil, errors.Errorf("failed to open eszip: %w", err)
	}
	defer eszipBytes.Close()

	result.compressedBody = bytes.NewBufferString(compressedEszipMagicId)
	brw := brotli.NewWriter(result.compressedBody)
	defer brw.Close()

	_, err = io.Copy(brw, eszipBytes)
	if err != nil {
		return nil, errors.Errorf("failed to compress brotli: %w", err)
	}

	return &result, nil
}

func deployFunction(ctx context.Context, projectRef, slug, entrypointUrl, importMapUrl string, verifyJWT bool, functionBody io.Reader) error {
	resp, err := utils.GetSupabase().V1GetAFunctionWithResponse(ctx, projectRef, slug)
	if err != nil {
		return errors.Errorf("failed to retrieve function: %w", err)
	}

	switch resp.StatusCode() {
	case http.StatusNotFound: // Function doesn't exist yet, so do a POST
		resp, err := utils.GetSupabase().CreateFunctionWithBodyWithResponse(ctx, projectRef, &api.CreateFunctionParams{
			Slug:           &slug,
			Name:           &slug,
			VerifyJwt:      &verifyJWT,
			ImportMapPath:  &importMapUrl,
			EntrypointPath: &entrypointUrl,
		}, eszipContentType, functionBody)
		if err != nil {
			return errors.Errorf("failed to create function: %w", err)
		}
		if resp.JSON201 == nil {
			return errors.New("Failed to create a new Function on the Supabase project: " + string(resp.Body))
		}
	case http.StatusOK: // Function already exists, so do a PATCH
		resp, err := utils.GetSupabase().V1UpdateAFunctionWithBodyWithResponse(ctx, projectRef, slug, &api.V1UpdateAFunctionParams{
			VerifyJwt:      &verifyJWT,
			ImportMapPath:  &importMapUrl,
			EntrypointPath: &entrypointUrl,
		}, eszipContentType, functionBody)
		if err != nil {
			return errors.Errorf("failed to update function: %w", err)
		}
		if resp.JSON200 == nil {
			return errors.New("Failed to update an existing Function's body on the Supabase project: " + string(resp.Body))
		}
	default:
		return errors.New("Unexpected error deploying Function: " + string(resp.Body))
	}

	fmt.Println("Deployed Function " + utils.Aqua(slug) + " on project " + utils.Aqua(projectRef))
	url := fmt.Sprintf("%s/project/%v/functions/%v/details", utils.GetSupabaseDashboardURL(), projectRef, slug)
	fmt.Println("You can inspect your deployment in the Dashboard: " + url)
	return nil
}

func deployOne(ctx context.Context, slug, projectRef, importMapPath string, noVerifyJWT *bool, fsys afero.Fs) error {
	// 1. Bundle Function.
	fmt.Println("Bundling " + utils.Bold(slug))
	fc := utils.GetFunctionConfig(slug, importMapPath, noVerifyJWT, fsys)
	eszip, err := bundleFunction(ctx, slug, fc.ImportMap, fsys)
	if err != nil {
		return err
	}
	// 2. Deploy new Function.
	functionSize := units.HumanSize(float64(eszip.compressedBody.Len()))
	fmt.Println("Deploying " + utils.Bold(slug) + " (script size: " + utils.Bold(functionSize) + ")")
	policy := backoff.WithContext(backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 3), ctx)
	return backoff.Retry(func() error {
		return deployFunction(
			ctx,
			projectRef,
			slug,
			"file://"+eszip.entrypointPath,
			"file://"+eszip.importMapPath,
			*fc.VerifyJWT,
			eszip.compressedBody,
		)
	}, policy)
}

func deployAll(ctx context.Context, slugs []string, projectRef, importMapPath string, noVerifyJWT *bool, fsys afero.Fs) error {
	// TODO: api has a race condition that prevents deploying in parallel
	for _, slug := range slugs {
		if err := deployOne(ctx, slug, projectRef, importMapPath, noVerifyJWT, fsys); err != nil {
			return err
		}
	}
	return nil
}
