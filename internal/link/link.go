package link

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
	"github.com/go-errors/errors"
	"github.com/jackc/pgconn"
	"github.com/jackc/pgx/v4"
	"github.com/spf13/afero"
	"github.com/spf13/viper"
	"github.com/supabase/cli/internal/migration/history"
	"github.com/supabase/cli/internal/utils"
	"github.com/supabase/cli/internal/utils/credentials"
	"github.com/supabase/cli/internal/utils/flags"
	"github.com/supabase/cli/internal/utils/tenant"
	"github.com/supabase/cli/pkg/api"
)

var updatedConfig ConfigCopy

type ConfigCopy struct {
	Api    interface{} `toml:"api"`
	Db     interface{} `toml:"db"`
	Pooler interface{} `toml:"db.pooler"`
}

func (c ConfigCopy) IsEmpty() bool {
	return c.Api == nil && c.Db == nil && c.Pooler == nil
}

func Run(ctx context.Context, projectRef string, fsys afero.Fs, options ...func(*pgx.ConnConfig)) error {
	// 1. Check service config
	keys, err := tenant.GetApiKeys(ctx, projectRef)
	if err != nil {
		return err
	}
	LinkServices(ctx, projectRef, keys.Anon, fsys)

	// 2. Check database connection
	config := flags.GetDbConfigOptionalPassword(projectRef)
	if len(config.Password) > 0 {
		if err := linkDatabase(ctx, config, options...); err != nil {
			return err
		}
		// Save database password
		if err := credentials.Set(projectRef, config.Password); err != nil {
			fmt.Fprintln(os.Stderr, "Failed to save database password:", err)
		}
	}

	// 3. Save project ref
	return utils.WriteFile(utils.ProjectRefPath, []byte(projectRef), fsys)
}

func PostRun(projectRef string, stdout io.Writer, fsys afero.Fs) error {
	fmt.Fprintln(stdout, "Finished "+utils.Aqua("supabase link")+".")
	if updatedConfig.IsEmpty() {
		return nil
	}
	fmt.Fprintln(os.Stderr, utils.Yellow("Warning:"), "Local config differs from linked project. Try updating", utils.Bold(utils.ConfigPath))
	enc := toml.NewEncoder(stdout)
	enc.Indent = ""
	if err := enc.Encode(updatedConfig); err != nil {
		return errors.Errorf("failed to marshal toml config: %w", err)
	}
	return nil
}

func LinkServices(ctx context.Context, projectRef, anonKey string, fsys afero.Fs) {
	// Ignore non-fatal errors linking services
	var wg sync.WaitGroup
	wg.Add(6)
	go func() {
		defer wg.Done()
		if err := linkDatabaseVersion(ctx, projectRef, fsys); err != nil && viper.GetBool("DEBUG") {
			fmt.Fprintln(os.Stderr, err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := linkPostgrest(ctx, projectRef); err != nil && viper.GetBool("DEBUG") {
			fmt.Fprintln(os.Stderr, err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := linkPooler(ctx, projectRef, fsys); err != nil && viper.GetBool("DEBUG") {
			fmt.Fprintln(os.Stderr, err)
		}
	}()
	api := tenant.NewTenantAPI(ctx, projectRef, anonKey)
	go func() {
		defer wg.Done()
		if err := linkPostgrestVersion(ctx, api, fsys); err != nil && viper.GetBool("DEBUG") {
			fmt.Fprintln(os.Stderr, err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := linkGotrueVersion(ctx, api, fsys); err != nil && viper.GetBool("DEBUG") {
			fmt.Fprintln(os.Stderr, err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := linkStorageVersion(ctx, api, fsys); err != nil && viper.GetBool("DEBUG") {
			fmt.Fprintln(os.Stderr, err)
		}
	}()
	wg.Wait()
}

func linkPostgrest(ctx context.Context, projectRef string) error {
	resp, err := utils.GetSupabase().V1GetPostgrestServiceConfigWithResponse(ctx, projectRef)
	if err != nil {
		return errors.Errorf("failed to get postgrest config: %w", err)
	}
	if resp.JSON200 == nil {
		return errors.Errorf("%w: %s", tenant.ErrAuthToken, string(resp.Body))
	}
	updateApiConfig(*resp.JSON200)
	return nil
}

func linkPostgrestVersion(ctx context.Context, api tenant.TenantAPI, fsys afero.Fs) error {
	version, err := api.GetPostgrestVersion(ctx)
	if err != nil {
		return err
	}
	return utils.WriteFile(utils.RestVersionPath, []byte(version), fsys)
}

func updateApiConfig(config api.PostgrestConfigWithJWTSecretResponse) {
	copy := utils.Config.Api
	copy.MaxRows = uint(config.MaxRows)
	copy.ExtraSearchPath = readCsv(config.DbExtraSearchPath)
	copy.Schemas = readCsv(config.DbSchema)
	changed := utils.Config.Api.MaxRows != copy.MaxRows ||
		!utils.SliceEqual(utils.Config.Api.ExtraSearchPath, copy.ExtraSearchPath) ||
		!utils.SliceEqual(utils.Config.Api.Schemas, copy.Schemas)
	if changed {
		updatedConfig.Api = copy
	}
}

func readCsv(line string) []string {
	var result []string
	tokens := strings.Split(line, ",")
	for _, t := range tokens {
		trimmed := strings.TrimSpace(t)
		if len(trimmed) > 0 {
			result = append(result, trimmed)
		}
	}
	return result
}

func linkGotrueVersion(ctx context.Context, api tenant.TenantAPI, fsys afero.Fs) error {
	version, err := api.GetGotrueVersion(ctx)
	if err != nil {
		return err
	}
	return utils.WriteFile(utils.GotrueVersionPath, []byte(version), fsys)
}

func linkStorageVersion(ctx context.Context, api tenant.TenantAPI, fsys afero.Fs) error {
	version, err := api.GetStorageVersion(ctx)
	if err != nil {
		return err
	}
	return utils.WriteFile(utils.StorageVersionPath, []byte(version), fsys)
}

func linkDatabase(ctx context.Context, config pgconn.Config, options ...func(*pgx.ConnConfig)) error {
	conn, err := utils.ConnectByConfig(ctx, config, options...)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	updatePostgresConfig(conn)
	// If `schema_migrations` doesn't exist on the remote database, create it.
	return history.CreateMigrationTable(ctx, conn)
}

func linkDatabaseVersion(ctx context.Context, projectRef string, fsys afero.Fs) error {
	version, err := tenant.GetDatabaseVersion(ctx, projectRef)
	if err != nil {
		return err
	}
	return utils.WriteFile(utils.PostgresVersionPath, []byte(version), fsys)
}

func updatePostgresConfig(conn *pgx.Conn) {
	serverVersion := conn.PgConn().ParameterStatus("server_version")
	// Safe to assume that supported Postgres version is 10.0 <= n < 100.0
	majorDigits := len(serverVersion)
	if majorDigits > 2 {
		majorDigits = 2
	}
	dbMajorVersion, err := strconv.ParseUint(serverVersion[:majorDigits], 10, 7)
	// Treat error as unchanged
	if err == nil && uint64(utils.Config.Db.MajorVersion) != dbMajorVersion {
		copy := utils.Config.Db
		copy.MajorVersion = uint(dbMajorVersion)
		updatedConfig.Db = copy
	}
}

func linkPooler(ctx context.Context, projectRef string, fsys afero.Fs) error {
	resp, err := utils.GetSupabase().V1GetProjectPgbouncerConfigWithResponse(ctx, projectRef)
	if err != nil {
		return errors.Errorf("failed to get pooler config: %w", err)
	}
	if resp.JSON200 == nil {
		return errors.Errorf("%w: %s", tenant.ErrAuthToken, string(resp.Body))
	}
	updatePoolerConfig(*resp.JSON200)
	if resp.JSON200.ConnectionString != nil {
		utils.Config.Db.Pooler.ConnectionString = *resp.JSON200.ConnectionString
		return utils.WriteFile(utils.PoolerUrlPath, []byte(utils.Config.Db.Pooler.ConnectionString), fsys)
	}
	return nil
}

func updatePoolerConfig(config api.V1PgbouncerConfigResponse) {
	copy := utils.Config.Db.Pooler
	if config.PoolMode != nil {
		copy.PoolMode = utils.PoolMode(*config.PoolMode)
	}
	if config.DefaultPoolSize != nil {
		copy.DefaultPoolSize = uint(*config.DefaultPoolSize)
	}
	if config.MaxClientConn != nil {
		copy.MaxClientConn = uint(*config.MaxClientConn)
	}
	changed := utils.Config.Db.Pooler.PoolMode != copy.PoolMode ||
		utils.Config.Db.Pooler.DefaultPoolSize != copy.DefaultPoolSize ||
		utils.Config.Db.Pooler.MaxClientConn != copy.MaxClientConn
	if changed {
		updatedConfig.Pooler = copy
	}
}
