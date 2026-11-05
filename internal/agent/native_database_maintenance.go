package agent

import (
	"context"
	"errors"
	"net"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
)

// Keep the original URL as the resource identity and as the application's
// runtime connection. Host-side maintenance can use the reviewed direct port.
func nativePostgresMaintenanceURL(source string, policy NativeDatabasePolicy) (string, error) {
	if policy.MaintenancePort == "" {
		return source, nil
	}
	if !validNativeMaintenancePort(policy) || validateNativeDatabaseURL(source, policy) != nil {
		return "", errors.New("native PostgreSQL maintenance URL differs from its reviewed pool")
	}
	parsed, err := url.Parse(source)
	if err != nil {
		return "", errors.New("native PostgreSQL maintenance URL is invalid")
	}
	parsed.Host = net.JoinHostPort(parsed.Hostname(), policy.MaintenancePort)
	return parsed.String(), nil
}

type nativePostgresBackend struct {
	address  string
	port     int
	version  int
	database uint32
	role     uint32
}

func readNativePostgresBackend(ctx context.Context, raw string) (nativePostgresBackend, error) {
	var identity nativePostgresBackend
	cfg, err := pgx.ParseConfig(raw)
	if err != nil {
		return identity, errors.New("native PostgreSQL maintenance identity is unavailable")
	}
	cfg.ConnectTimeout = 4 * time.Second
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.RuntimeParams["application_name"] = "lidza-native-maintenance-identity"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return identity, errors.New("native PostgreSQL maintenance identity connection failed")
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return identity, errors.New("native PostgreSQL maintenance identity requires a read-only transaction")
	}
	var readOnly string
	err = tx.QueryRow(ctx, `SELECT host(inet_server_addr()), inet_server_port(), current_setting('server_version_num')::integer,
		(SELECT oid FROM pg_catalog.pg_database WHERE datname=current_database()),
		(SELECT oid FROM pg_catalog.pg_roles WHERE rolname=current_user), current_setting('transaction_read_only')`).
		Scan(&identity.address, &identity.port, &identity.version, &identity.database, &identity.role, &readOnly)
	if err != nil || readOnly != "on" || identity.database == 0 || identity.role == 0 || tx.Commit(ctx) != nil {
		return identity, errors.New("native PostgreSQL maintenance backend identity could not be proven")
	}
	return identity, nil
}

func verifyNativePostgresMaintenance(ctx context.Context, source string, policy NativeDatabasePolicy) error {
	direct, err := nativePostgresMaintenanceURL(source, policy)
	if err != nil {
		return err
	}
	pooled, err := readNativePostgresBackend(ctx, source)
	if err != nil {
		return err
	}
	maintenance, err := readNativePostgresBackend(ctx, direct)
	if err != nil || pooled != maintenance || maintenance.address != "127.0.0.1" ||
		maintenance.port != 5432 || maintenance.version/10000 != policy.PostgresMajor {
		return errors.New("native PostgreSQL maintenance backend differs from the application pool")
	}
	return nil
}
