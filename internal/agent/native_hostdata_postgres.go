package agent

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Host-level PostgreSQL for applications the agent creates. Every application
// gets its own role and database on the host's default cluster:
//
//   - the role is LOGIN only (no superuser, database/role creation,
//     replication or row-level-security bypass) with a SCRAM verifier the
//     agent computes itself, so the plaintext never reaches the server log;
//   - the database is owned by that role, PUBLIC loses CONNECT/TEMPORARY on it
//     and every privilege on its public schema;
//   - a managed pg_hba.conf block admits the role only to its own database,
//     over the Unix socket or loopback with scram-sha-256, and rejects it
//     everywhere else. The block precedes every other rule (first match wins),
//     and the agent never adds a rule for a non-loopback network.
//
// SQL is sent to psql on stdin, run as the postgres OS user through the local
// socket (peer authentication); no credential appears in a process argument.

const (
	nativePGSocketDir   = "/var/run/postgresql"
	nativePGMinMajor    = 16
	nativePGMaxMajor    = 18
	nativePGDGDefault   = 17
	nativeHBABegin      = "# BEGIN lidza-deploy application access (managed; do not edit inside this block)"
	nativeHBAEnd        = "# END lidza-deploy application access"
	nativeSCRAMIterates = 4096
)

// nativePGName is the role and database name for an application id: "app_"
// and the id with dashes as underscores. Ids are [a-z][a-z0-9-]{0,47}, so the
// mapping is injective and stays within PostgreSQL's 63-byte identifier limit.
func nativePGName(id string) (string, error) {
	if !idPattern.MatchString(id) {
		return "", errors.New("invalid application id for a database name")
	}
	return "app_" + strings.ReplaceAll(id, "-", "_"), nil
}

// pgQuoteIdent quotes an SQL identifier ("" doubles an embedded quote). NUL
// cannot be represented and is refused.
func pgQuoteIdent(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, 0) || len(name) > 63 {
		return "", errors.New("invalid SQL identifier")
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`, nil
}

// pgQuoteLiteral quotes an SQL string literal like libpq's PQescapeLiteral:
// single quotes are doubled, and a value with a backslash uses the E-prefixed form
// with doubled backslashes, so the result is independent of
// standard_conforming_strings.
func pgQuoteLiteral(value string) (string, error) {
	if strings.ContainsRune(value, 0) {
		return "", errors.New("invalid SQL literal")
	}
	quoted := "'" + strings.ReplaceAll(value, "'", "''") + "'"
	if strings.Contains(value, `\`) {
		quoted = "E'" + strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), "'", "''") + "'"
	}
	return quoted, nil
}

// nativeSCRAMVerifier returns the SCRAM-SHA-256 verifier PostgreSQL stores
// for password (RFC 5802/7677). Sending the verifier instead of the password
// keeps the plaintext out of statement logs and pg_stat_activity.
func nativeSCRAMVerifier(password string, salt []byte, iterations int) (string, error) {
	if len(salt) < 16 || iterations < 4096 {
		return "", errors.New("SCRAM verifier needs a 16-byte salt and at least 4096 iterations")
	}
	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return "", err
	}
	clientKey := nativeHMAC(salted, "Client Key")
	stored := sha256.Sum256(clientKey)
	serverKey := nativeHMAC(salted, "Server Key")
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations, b64(salt), b64(stored[:]), b64(serverKey)), nil
}

func nativeHMAC(key []byte, message string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(message))
	return mac.Sum(nil)
}

// nativeRandomSecret is a 32-byte random secret as 64 lowercase hex digits:
// safe in URLs, configuration files and SASLprep without escaping.
func nativeRandomSecret() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// renderNativePGProvisionSQL is the idempotent psql script that creates or
// repairs one application's role and database. CREATE DATABASE cannot run in
// a transaction or DO block, so conditional creation uses \gexec.
func renderNativePGProvisionSQL(name, verifier string) (string, error) {
	ident, err := pgQuoteIdent(name)
	if err != nil {
		return "", err
	}
	nameLiteral, err := pgQuoteLiteral(name)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(verifier, "SCRAM-SHA-256$") {
		return "", errors.New("role password must be a SCRAM-SHA-256 verifier")
	}
	verifierLiteral, err := pgQuoteLiteral(verifier)
	if err != nil {
		return "", err
	}
	createRole, _ := pgQuoteLiteral("CREATE ROLE " + ident)
	createDatabase, _ := pgQuoteLiteral("CREATE DATABASE " + ident + " OWNER " + ident + " TEMPLATE template0 ENCODING 'UTF8'")
	var b strings.Builder
	fmt.Fprintf(&b, "SELECT %s WHERE NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = %s)\\gexec\n", createRole, nameLiteral)
	fmt.Fprintf(&b, "ALTER ROLE %s WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS INHERIT PASSWORD %s;\n", ident, verifierLiteral)
	fmt.Fprintf(&b, "SELECT %s WHERE NOT EXISTS (SELECT 1 FROM pg_catalog.pg_database WHERE datname = %s)\\gexec\n", createDatabase, nameLiteral)
	fmt.Fprintf(&b, "ALTER DATABASE %s OWNER TO %s;\n", ident, ident)
	fmt.Fprintf(&b, "REVOKE CONNECT, TEMPORARY ON DATABASE %s FROM PUBLIC;\n", ident)
	fmt.Fprintf(&b, "GRANT CONNECT, TEMPORARY ON DATABASE %s TO %s;\n", ident, ident)
	fmt.Fprintf(&b, "\\connect %s\n", ident)
	b.WriteString("REVOKE ALL ON SCHEMA public FROM PUBLIC;\n")
	fmt.Fprintf(&b, "GRANT USAGE, CREATE ON SCHEMA public TO %s;\n", ident)
	return b.String(), nil
}

// renderNativePGReleaseSQL drops one application's database and role. Login
// is disabled and sessions are terminated first; WITH (FORCE) covers late
// connections. Every statement tolerates an already removed object.
func renderNativePGReleaseSQL(name string) (string, error) {
	ident, err := pgQuoteIdent(name)
	if err != nil {
		return "", err
	}
	literal, _ := pgQuoteLiteral(name)
	nologin, _ := pgQuoteLiteral("ALTER ROLE " + ident + " NOLOGIN")
	var b strings.Builder
	fmt.Fprintf(&b, "SELECT %s WHERE EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = %s)\\gexec\n", nologin, literal)
	fmt.Fprintf(&b, "SELECT count(pg_catalog.pg_terminate_backend(pid)) FROM pg_catalog.pg_stat_activity WHERE datname = %s OR usename = %s;\n", literal, literal)
	fmt.Fprintf(&b, "DROP DATABASE IF EXISTS %s WITH (FORCE);\n", ident)
	fmt.Fprintf(&b, "DROP ROLE IF EXISTS %s;\n", ident)
	return b.String(), nil
}

// nativePGURL is the application's DATABASE_URL: password authentication
// over loopback. The cluster listens on localhost only by default; sslmode is
// disabled because the connection never leaves the host.
func nativePGURL(name, password string, port int) string {
	return fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/%s?sslmode=disable", name, password, port, name)
}

// pgCluster is one row of pg_lsclusters.
type pgCluster struct {
	Major    int
	Name     string
	Port     int
	Online   bool
	Recovery bool
	Owner    string
	DataDir  string
}

func (c pgCluster) ID() string { return strconv.Itoa(c.Major) + "/" + c.Name }

var pgClusterName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,63}$`)

// parsePGClusters reads `pg_lsclusters --no-header` output. Rows with older
// dotted versions or unexpected fields are skipped.
func parsePGClusters(out string) []pgCluster {
	clusters := []pgCluster{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		major, err := strconv.Atoi(fields[0])
		port, perr := strconv.Atoi(fields[2])
		if err != nil || perr != nil || port < 1 || port > 65535 || !pgClusterName.MatchString(fields[1]) {
			continue
		}
		status := strings.Split(fields[3], ",")
		clusters = append(clusters, pgCluster{Major: major, Name: fields[1], Port: port, Online: status[0] == "online", Recovery: slices.Contains(status, "recovery"), Owner: fields[4], DataDir: fields[5]})
	}
	return clusters
}

// chooseNativePGCluster picks the cluster applications use: the configured
// version/port when set, otherwise the online primary on the lowest port (the
// distribution default cluster). It never picks a stopped cluster or standby.
func chooseNativePGCluster(clusters []pgCluster, version, port int) (pgCluster, error) {
	candidates := []pgCluster{}
	stopped := false
	for _, c := range clusters {
		if version != 0 && c.Major != version || port != 0 && c.Port != port {
			continue
		}
		if !c.Online || c.Recovery || c.Owner != "postgres" {
			stopped = true
			continue
		}
		candidates = append(candidates, c)
	}
	if len(candidates) == 0 {
		if stopped {
			return pgCluster{}, errors.New("the matching PostgreSQL cluster is stopped, a standby or not owned by postgres; it is left unchanged")
		}
		return pgCluster{}, errNativePGNoCluster
	}
	slices.SortFunc(candidates, func(a, b pgCluster) int { return a.Port - b.Port })
	chosen := candidates[0]
	if chosen.Major < nativePGMinMajor || chosen.Major > nativePGMaxMajor {
		return pgCluster{}, fmt.Errorf("PostgreSQL %d is outside the supported %d–%d range; set native.host_data.postgres_version to install a supported version", chosen.Major, nativePGMinMajor, nativePGMaxMajor)
	}
	return chosen, nil
}

var errNativePGNoCluster = errors.New("no PostgreSQL cluster matches")

// nativeHBAEntry admits one role to its own database.
type nativeHBAEntry struct {
	Role     string
	Database string
}

// renderNativeHBABlock renders the managed pg_hba.conf block. For each
// application: the role may reach only its own database and that database's
// staging copy (restores build the new contents there before renaming it
// into place), over the socket or loopback with scram-sha-256; every other
// combination for that role is
// rejected, even if a later rule (for example a broad network rule added by
// an administrator) would match. Nothing else on the server is affected.
func renderNativeHBABlock(entries []nativeHBAEntry) string {
	if len(entries) == 0 {
		return ""
	}
	sorted := slices.Clone(entries)
	slices.SortFunc(sorted, func(a, b nativeHBAEntry) int { return strings.Compare(a.Role, b.Role) })
	var b strings.Builder
	b.WriteString(nativeHBABegin + "\n")
	for _, e := range sorted {
		databases := e.Database + "," + e.Database + pgStagingSuffix
		fmt.Fprintf(&b, "local   %-24s %-24s %-18s scram-sha-256\n", databases, e.Role, "")
		fmt.Fprintf(&b, "host    %-24s %-24s %-18s scram-sha-256\n", databases, e.Role, "127.0.0.1/32")
		fmt.Fprintf(&b, "host    %-24s %-24s %-18s scram-sha-256\n", databases, e.Role, "::1/128")
		fmt.Fprintf(&b, "local   %-24s %-24s %-18s reject\n", "all", e.Role, "")
		fmt.Fprintf(&b, "host    %-24s %-24s %-18s reject\n", "all", e.Role, "all")
	}
	b.WriteString(nativeHBAEnd + "\n")
	return b.String()
}

// spliceNativeHBA replaces the managed block in a pg_hba.conf, inserting it
// before the first rule so it takes precedence. Everything outside the block
// is preserved byte for byte. An empty block removes it.
func spliceNativeHBA(existing, block string) (string, error) {
	lines := strings.SplitAfter(existing, "\n")
	kept := make([]string, 0, len(lines))
	inside := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == nativeHBABegin:
			if inside {
				return "", errors.New("pg_hba.conf has a nested managed block; edit it manually")
			}
			inside = true
		case trimmed == nativeHBAEnd:
			if !inside {
				return "", errors.New("pg_hba.conf has an unmatched managed block end; edit it manually")
			}
			inside = false
		case !inside:
			kept = append(kept, line)
		}
	}
	if inside {
		return "", errors.New("pg_hba.conf has an unterminated managed block; edit it manually")
	}
	if block == "" {
		return strings.Join(kept, ""), nil
	}
	insert := len(kept)
	for i, line := range kept {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			insert = i
			break
		}
	}
	if insert == len(kept) && insert > 0 && !strings.HasSuffix(kept[insert-1], "\n") {
		kept[insert-1] += "\n"
	}
	out := slices.Concat(kept[:insert], []string{block}, kept[insert:])
	return strings.Join(out, ""), nil
}

// nativeDistributionPGMajor reads the major version the distribution's
// "postgresql" metapackage depends on from `apt-cache show` output.
func nativeDistributionPGMajor(out string) int {
	match := regexp.MustCompile(`(?m)^Depends:.*\bpostgresql-(\d+)\b`).FindStringSubmatch(out)
	if match == nil {
		return 0
	}
	major, _ := strconv.Atoi(match[1])
	return major
}

// nativeHostDataPolicyDigest is the binding's configuration identity: the
// cluster, port and names the agent provisioned. It changes only when the
// agent re-provisions the database somewhere else.
func nativeHostDataPolicyDigest(cluster string, port int, name string) string {
	sum := sha256.Sum256([]byte("lidza-host-postgres\x00" + cluster + "\x00" + strconv.Itoa(port) + "\x00" + name + "\x00" + name))
	return hex.EncodeToString(sum[:])
}

// ---- host operations -------------------------------------------------------

// psql runs a script as the postgres OS user against the cluster's socket.
func (h nativeHostData) psql(ctx context.Context, port int, script string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := h.run(ctx, script, "/usr/sbin/runuser", "-u", "postgres", "--", "/usr/bin/psql", "-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-h", nativePGSocketDir, "-p", strconv.Itoa(port), "-d", "postgres")
	if err != nil {
		return out, fmt.Errorf("psql on port %d failed: %w %s", port, err, nativeTrimOutput(out))
	}
	return strings.TrimSpace(out), nil
}

// ensurePostgres returns the cluster to use, installing PostgreSQL first when
// the host has none. Running clusters are never restarted.
func (h nativeHostData) ensurePostgres(ctx context.Context) (pgCluster, error) {
	version, port := h.cfg.PostgresVersion, h.cfg.PostgresPort
	clusters, _ := h.listClusters(ctx)
	cluster, err := chooseNativePGCluster(clusters, version, port)
	if !errors.Is(err, errNativePGNoCluster) {
		return cluster, err
	}
	if port != 0 {
		return pgCluster{}, fmt.Errorf("no online PostgreSQL cluster listens on the configured port %d", port)
	}
	if version == 0 && len(clusters) == 0 {
		// Prefer the distribution's PostgreSQL; use the PostgreSQL project's
		// repository only when the distribution's version is unsupported.
		if _, err := h.run(ctx, "", "/usr/bin/apt-get", "-o", "DPkg::Lock::Timeout=300", "update", "-q"); err != nil {
			return pgCluster{}, errors.New("package index update failed")
		}
		show, _ := h.run(ctx, "", "/usr/bin/apt-cache", "show", "--no-all-versions", "postgresql")
		distribution := nativeDistributionPGMajor(show)
		if distribution >= nativePGMinMajor && distribution <= nativePGMaxMajor {
			if out, err := h.aptInstall(ctx, "postgresql"); err != nil {
				return pgCluster{}, fmt.Errorf("PostgreSQL installation failed: %s", nativeTrimOutput(out))
			}
			version = distribution
		} else {
			version = nativePGDGDefault
		}
	}
	if version != 0 && !h.fileExists(filepath.Join("/usr/lib/postgresql", strconv.Itoa(version), "bin/postgres")) {
		if version < nativePGMinMajor || version > nativePGMaxMajor {
			return pgCluster{}, fmt.Errorf("PostgreSQL %d is unsupported; choose %d–%d", version, nativePGMinMajor, nativePGMaxMajor)
		}
		if err := h.ensurePGDG(ctx); err != nil {
			return pgCluster{}, err
		}
		if out, err := h.aptInstall(ctx, "postgresql-"+strconv.Itoa(version)); err != nil {
			return pgCluster{}, fmt.Errorf("PostgreSQL %d installation failed: %s", version, nativeTrimOutput(out))
		}
	}
	clusters, _ = h.listClusters(ctx)
	cluster, err = chooseNativePGCluster(clusters, version, 0)
	if errors.Is(err, errNativePGNoCluster) {
		// createcluster.conf can disable the package's default cluster.
		if out, err := h.run(ctx, "", "/usr/bin/pg_createcluster", strconv.Itoa(version), "main", "--start"); err != nil {
			return pgCluster{}, fmt.Errorf("PostgreSQL cluster creation failed: %s", nativeTrimOutput(out))
		}
		clusters, _ = h.listClusters(ctx)
		cluster, err = chooseNativePGCluster(clusters, version, 0)
	}
	return cluster, err
}

func (h nativeHostData) listClusters(ctx context.Context) ([]pgCluster, error) {
	if !h.fileExists("/usr/bin/pg_lsclusters") {
		return nil, nil
	}
	out, err := h.run(ctx, "", "/usr/bin/pg_lsclusters", "--no-header")
	if err != nil {
		return nil, err
	}
	return parsePGClusters(out), nil
}

// ensurePGDG adds the PostgreSQL project's apt repository with the signed
// setup script that the distribution's postgresql-common package ships.
func (h nativeHostData) ensurePGDG(ctx context.Context) error {
	if h.fileExists("/etc/apt/sources.list.d/pgdg.list") || h.fileExists("/etc/apt/sources.list.d/pgdg.sources") {
		return nil
	}
	if out, err := h.aptInstall(ctx, "postgresql-common", "ca-certificates"); err != nil {
		return fmt.Errorf("postgresql-common installation failed: %s", nativeTrimOutput(out))
	}
	if out, err := h.run(ctx, "\n", "/usr/share/postgresql-common/pgdg/apt.postgresql.org.sh", "-y"); err != nil {
		return fmt.Errorf("PostgreSQL repository setup failed: %s", nativeTrimOutput(out))
	}
	return nil
}

func (h nativeHostData) aptInstall(ctx context.Context, packages ...string) (string, error) {
	args := append([]string{"-o", "DPkg::Lock::Timeout=300", "-o", "Dpkg::Options::=--force-confold", "install", "-y", "-q", "--no-install-recommends"}, packages...)
	return h.run(ctx, "", "/usr/bin/apt-get", args...)
}

// ensureHBA writes the managed block for every application on this cluster,
// validates the file with pg_hba_file_rules before activating it and reloads
// the configuration (no restart). An invalid result restores the old file.
func (h nativeHostData) ensureHBA(ctx context.Context, port int, entries []nativeHBAEntry) error {
	path, err := h.psql(ctx, port, "SHOW hba_file;\n")
	if err != nil {
		return err
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("PostgreSQL reported an unexpected pg_hba.conf path")
	}
	info, err := os.Lstat(h.path(path))
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return errors.New("pg_hba.conf is unavailable or not a regular file")
	}
	old, err := os.ReadFile(h.path(path))
	if err != nil {
		return err
	}
	next, err := spliceNativeHBA(string(old), renderNativeHBABlock(entries))
	if err != nil {
		return err
	}
	if next == string(old) {
		return nil
	}
	// Compare with the errors already present, so an unrelated pre-existing
	// problem is not attributed to (or hidden by) the managed block.
	const countErrors = "SELECT count(*) FROM pg_catalog.pg_hba_file_rules WHERE error IS NOT NULL;\n"
	before, err := h.psql(ctx, port, countErrors)
	if err != nil {
		return err
	}
	if err = nativeWriteFileLike(h.path(path), []byte(next), info); err != nil {
		return err
	}
	after, err := h.psql(ctx, port, countErrors)
	if err != nil || after != before {
		_ = nativeWriteFileLike(h.path(path), old, info)
		return errors.New("updated pg_hba.conf did not validate; the previous file was restored")
	}
	if _, err = h.psql(ctx, port, "SELECT pg_catalog.pg_reload_conf();\n"); err != nil {
		return err
	}
	return nil
}

// nativeWriteFileLike atomically replaces path, keeping the old owner and mode.
func nativeWriteFileLike(path string, data []byte, like os.FileInfo) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".lidza-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if stat, ok := like.Sys().(*syscall.Stat_t); ok {
		if err = os.Chown(tmp.Name(), int(stat.Uid), int(stat.Gid)); err != nil {
			return err
		}
	}
	if err = os.Chmod(tmp.Name(), like.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// nativePGDumpDigest hashes the cluster version's pg_dump the way the native
// backup helper verifies it: a root-owned regular file that only root can
// change, opened without following symlinks.
func nativePGDumpDigest(path string) (string, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", errors.New("pg_dump for the cluster version is unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0022 != 0 || info.Size() > 64<<20 {
		return "", errors.New("pg_dump is not a trusted regular file")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != 0 {
		return "", errors.New("pg_dump must be owned by root")
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, io.LimitReader(file, 64<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func nativePGDumpPath(major int) string {
	return filepath.Join("/usr/lib/postgresql", strconv.Itoa(major), "bin/pg_dump")
}

func nativeTrimOutput(out string) string {
	out = strings.TrimSpace(out)
	if len(out) > 400 {
		out = out[len(out)-400:]
	}
	return out
}
