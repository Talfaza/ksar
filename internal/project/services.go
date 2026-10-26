package project

import (
	"bufio"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Services: many projects need a database server next to them (a Go API
// on MySQL, a Django app on PostgreSQL, a Node app with Redis). The run
// installs it from the image's own distribution mirror and starts it
// inside the same VM, on 127.0.0.1 only, before the project's command.
//
// Credentials come from the project when it names a local server (a DSN
// or URL in .env files or docker-compose.yml pointing at localhost or a
// compose service name), so the project connects with what it already
// expects; otherwise from defaults, exported as DATABASE_URL and friends.
// Every value taken from the project must match a strict character set:
// project text never reaches the shell or SQL unchecked.

type service struct {
	name   string // mysql, postgres, redis
	label  string
	port   string
	driver *regexp.Regexp // in a manifest: the project uses it
}

var services = []service{
	{"mysql", "MySQL (MariaDB)", "3306", regexp.MustCompile(`(?i)driver/mysql|go-sql-driver/mysql|"mysql2?"|pymysql|mysqlclient|mysql-connector|mysqli|pdo_mysql`)},
	{"postgres", "PostgreSQL", "5432", regexp.MustCompile(`(?i)driver/postgres|jackc/pgx|lib/pq|"pg"|"postgres"|psycopg|asyncpg|"sequelize"`)},
	{"redis", "Redis", "6379", regexp.MustCompile(`(?i)go-redis|redis/go-redis|"redis"|"ioredis"|(?m)^redis\b|django-redis|celery\[redis\]`)},
}

// mongoDriver: MongoDB isn't in the distributions' own repositories.
var mongoDriver = regexp.MustCompile(`(?i)mongo-driver|"mongoose"|"mongodb"|pymongo|\bmotor\b`)

var manifests = []string{"go.mod", "package.json", "requirements.txt", "pyproject.toml", "Pipfile", "Gemfile", "composer.json"}

// dbCreds is a database a project expects. Every field is checked by safe.
type dbCreds struct {
	user, password, name, host string
	fromProject                bool
}

var (
	safeIdent = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,62}$`)
	safePass  = regexp.MustCompile(`^[A-Za-z0-9_.@%+=:,!#^*()\[\]{}~-]{0,64}$`)
	// user:pass@tcp(host:port)/name (Go's MySQL driver)
	goDSN = regexp.MustCompile(`([A-Za-z0-9_.-]+)(?::([^@\s"']*))?@tcp\(([A-Za-z0-9_.-]+)(?::\d+)?\)/([A-Za-z0-9_.-]+)`)
	// mysql://, postgres://, postgresql://, redis:// URLs
	dbURL   = regexp.MustCompile(`(mysql|mariadb|postgres|postgresql|redis)(?:\+\w+)?://(?:([A-Za-z0-9_.-]*)(?::([^@\s"']*))?@)?([A-Za-z0-9_.-]+)(?::\d+)?(?:/([A-Za-z0-9_.-]*))?`)
	envLine = regexp.MustCompile(`^\s*(?:export\s+|-\s+)?([A-Z][A-Z0-9_]*)\s*[=:]\s*["']?([^"'#\s]*)`)
)

func (c dbCreds) safe() bool {
	return safeIdent.MatchString(c.user) && safeIdent.MatchString(c.name) && safeIdent.MatchString(c.host) && safePass.MatchString(c.password)
}

// localHost: a host the project's database is reached at that can be made
// to point here: loopback, or a compose service name (no dots).
func localHost(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "" || (safeIdent.MatchString(h) && !strings.Contains(h, "."))
}

// projectServices lists the services the given folders' manifests use, in
// a stable order, and whether MongoDB is used too.
func projectServices(root string, dirs []string) (names []string, mongo bool) {
	seen := map[string]bool{}
	for _, d := range dirs {
		for _, m := range manifests {
			text := string(readSmall(filepath.Join(root, filepath.FromSlash(d)), m))
			if text == "" {
				continue
			}
			for _, s := range services {
				if s.driver.MatchString(text) {
					seen[s.name] = true
				}
			}
			mongo = mongo || mongoDriver.MatchString(text)
		}
	}
	for _, s := range services {
		if seen[s.name] {
			names = append(names, s.name)
		}
	}
	return names, mongo
}

// expectedCreds reads what the project expects of a service from its env
// and compose files, in the given folders and the top.
func expectedCreds(root string, dirs []string, name string) dbCreds {
	def := dbCreds{user: "app", password: "app", name: "app", host: "localhost"}
	if name == "redis" {
		def = dbCreds{host: "localhost"}
	}
	files := []string{".env", ".env.local", ".env.development", ".env.example", ".env.sample", "docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"}
	for _, d := range append(dirs, ".") {
		for _, f := range files {
			text := string(readSmall(filepath.Join(root, filepath.FromSlash(d)), f))
			if text == "" {
				continue
			}
			if c, ok := credsIn(text, name); ok && c.safe() && localHost(c.host) {
				c.fromProject = true
				return c
			}
		}
	}
	return def
}

func credsIn(text, name string) (dbCreds, bool) {
	scheme := map[string]string{"mysql": "mysql mariadb", "postgres": "postgres postgresql", "redis": "redis"}[name]
	for _, m := range dbURL.FindAllStringSubmatch(text, -1) {
		if strings.Contains(scheme, m[1]) {
			c := dbCreds{user: m[2], password: m[3], host: m[4], name: m[5]}
			if name != "redis" && (c.user == "" || c.name == "") {
				continue
			}
			if name == "redis" {
				c.user, c.name = "default", "0"
			}
			return c, true
		}
	}
	if name == "mysql" {
		if m := goDSN.FindStringSubmatch(text); m != nil {
			return dbCreds{user: m[1], password: m[2], host: m[3], name: m[4]}, true
		}
	}
	if name == "redis" {
		return dbCreds{}, false
	}
	// KEY=value pairs: DB_*, MYSQL_*, POSTGRES_*, PG*.
	kv := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		if m := envLine.FindStringSubmatch(sc.Text()); m != nil {
			kv[m[1]] = m[2]
		}
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v := kv[k]; v != "" {
				return v
			}
		}
		return ""
	}
	var c dbCreds
	if name == "mysql" {
		c = dbCreds{user: pick("MYSQL_USER", "DB_USER", "DB_USERNAME", "DATABASE_USER"), password: pick("MYSQL_PASSWORD", "DB_PASSWORD", "DB_PASS", "DATABASE_PASSWORD"),
			name: pick("MYSQL_DATABASE", "DB_NAME", "DB_DATABASE", "DATABASE_NAME"), host: pick("MYSQL_HOST", "DB_HOST", "DATABASE_HOST")}
	} else {
		c = dbCreds{user: pick("POSTGRES_USER", "PGUSER", "DB_USER", "DB_USERNAME", "DATABASE_USER"), password: pick("POSTGRES_PASSWORD", "PGPASSWORD", "DB_PASSWORD", "DB_PASS", "DATABASE_PASSWORD"),
			name: pick("POSTGRES_DB", "PGDATABASE", "DB_NAME", "DB_DATABASE", "DATABASE_NAME"), host: pick("POSTGRES_HOST", "PGHOST", "DB_HOST", "DATABASE_HOST")}
	}
	if c.user == "" || c.name == "" {
		return dbCreds{}, false
	}
	if c.host == "" {
		c.host = "localhost"
	}
	return c, true
}

// alpineImage: the image's distribution is Alpine (apk), not Debian (apt).
func alpineImage(ref string) bool { return strings.Contains(ref, "alpine") }

// mirrors are the hosts a service install needs, by distribution.
func mirrors(ref string) []string {
	if alpineImage(ref) {
		return []string{"dl-cdn.alpinelinux.org"}
	}
	return []string{"deb.debian.org"}
}

// serviceSetup is the shell that installs, starts and prepares the
// services. Every value in it was checked by dbCreds.safe.
func serviceSetup(ref string, names []string, creds map[string]dbCreds) string {
	alpine := alpineImage(ref)
	var pkgs []string
	for _, n := range names {
		switch {
		case n == "mysql" && alpine:
			pkgs = append(pkgs, "mariadb", "mariadb-client")
		case n == "mysql":
			pkgs = append(pkgs, "mariadb-server")
		case n == "postgres":
			pkgs = append(pkgs, "postgresql")
		case n == "redis" && alpine:
			pkgs = append(pkgs, "redis")
		case n == "redis":
			pkgs = append(pkgs, "redis-server")
		}
	}
	var steps []string
	if alpine {
		steps = append(steps, "apk add -q --no-progress "+strings.Join(pkgs, " ")+" >/dev/null 2>&1")
	} else {
		steps = append(steps, "apt-get update -qq >/dev/null",
			"DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends "+strings.Join(pkgs, " ")+" >/dev/null 2>&1")
	}
	// Compose service names (db, mysql, ...) resolve to this VM.
	var hosts []string
	for _, n := range names {
		if h := creds[n].host; h != "" && h != "localhost" && h != "127.0.0.1" {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) > 0 {
		sort.Strings(hosts)
		steps = append(steps, "echo '127.0.0.1 "+strings.Join(hosts, " ")+"' >> /etc/hosts")
	}
	wait := func(check string) string {
		return "for i in $(seq 60); do " + check + " >/dev/null 2>&1 && break; sleep 0.5; done"
	}
	// SQL goes to the client on stdin, inside one pair of double quotes:
	// only " and ` need escaping there, since checked values can't hold
	// $ or \.
	sql := func(stmts ...string) string {
		q := strings.NewReplacer(`"`, `\"`, "`", "\\`").Replace(strings.Join(stmts, "; ") + ";")
		return `echo "` + q + `"`
	}
	for _, n := range names {
		c := creds[n]
		switch n {
		case "mysql":
			if alpine {
				steps = append(steps, "mariadb-install-db --user=mysql --datadir=/var/lib/mysql >/dev/null")
			}
			grant := []string{"CREATE DATABASE IF NOT EXISTS `" + c.name + "`"}
			if c.user == "root" {
				grant = append(grant, "ALTER USER 'root'@'localhost' IDENTIFIED BY '"+c.password+"'")
			} else {
				for _, h := range []string{"localhost", "127.0.0.1"} {
					grant = append(grant, "CREATE USER IF NOT EXISTS '"+c.user+"'@'"+h+"' IDENTIFIED BY '"+c.password+"'",
						"GRANT ALL ON `"+c.name+"`.* TO '"+c.user+"'@'"+h+"'")
				}
			}
			// The package may have started the server already.
			steps = append(steps, "mkdir -p /run/mysqld && chown mysql /run/mysqld",
				// Alpine's configuration has skip-networking.
				"(mariadb-admin ping >/dev/null 2>&1 || (mariadbd-safe --user=mysql --skip-networking=0 --bind-address=127.0.0.1 >/dev/null 2>&1 &))",
				wait("mariadb-admin ping"), sql(grant...)+" | mariadb")
		case "postgres":
			if alpine {
				steps = append(steps, "mkdir -p /run/postgresql /var/lib/postgresql/data && chown postgres /run/postgresql /var/lib/postgresql/data",
					"su postgres -c 'initdb -A trust -D /var/lib/postgresql/data >/dev/null'",
					"su postgres -c \"pg_ctl -D /var/lib/postgresql/data -o '-c listen_addresses=127.0.0.1' -w start >/dev/null\"")
			} else {
				steps = append(steps, "(pg_ctlcluster $(ls /etc/postgresql) main start 2>/dev/null; true)")
			}
			role := `CREATE ROLE "` + c.user + `" LOGIN SUPERUSER PASSWORD '` + c.password + `'`
			if c.user == "postgres" {
				role = `ALTER ROLE postgres PASSWORD '` + c.password + `'`
			}
			// psql carries on past "already exists".
			steps = append(steps, wait("su postgres -c 'psql -c \"select 1\"'"),
				sql(role, `CREATE DATABASE "`+c.name+`" OWNER "`+c.user+`"`)+" | su postgres -c 'psql -q' >/dev/null 2>&1")
		case "redis":
			steps = append(steps, "(redis-cli ping >/dev/null 2>&1 || redis-server --bind 127.0.0.1 --daemonize yes >/dev/null)", wait("redis-cli ping"))
		}
	}
	return strings.Join(steps, " && ")
}

// serviceEnv is what the project is told about its services.
func serviceEnv(names []string, creds map[string]dbCreds) []string {
	var env []string
	for _, n := range names {
		c := creds[n]
		switch n {
		case "mysql":
			env = append(env, "DATABASE_URL=mysql://"+c.user+":"+c.password+"@127.0.0.1:3306/"+c.name,
				"MYSQL_HOST=127.0.0.1", "MYSQL_USER="+c.user, "MYSQL_PASSWORD="+c.password, "MYSQL_DATABASE="+c.name)
		case "postgres":
			env = append(env, "DATABASE_URL=postgres://"+c.user+":"+c.password+"@127.0.0.1:5432/"+c.name,
				"PGHOST=127.0.0.1", "PGUSER="+c.user, "PGPASSWORD="+c.password, "PGDATABASE="+c.name)
		case "redis":
			env = append(env, "REDIS_URL=redis://127.0.0.1:6379/0")
		}
	}
	return env
}

// withServices gives an option the services its folders use.
func withServices(root string, o Option, dirs []string) Option {
	names, mongo := projectServices(root, dirs)
	if mongo {
		o.Note = joinNote(o.Note, "It uses MongoDB, which isn't in the distributions' own repositories, so it can't be started here: the parts that need it will stop at connecting.")
	}
	if len(names) == 0 {
		return o
	}
	creds := map[string]dbCreds{}
	var labels, from []string
	for _, n := range names {
		creds[n] = expectedCreds(root, dirs, n)
		for _, s := range services {
			if s.name == n {
				labels = append(labels, s.label)
			}
		}
		if creds[n].fromProject {
			from = append(from, n)
		}
	}
	// The project learns where its services are after they're set up: in
	// the run's environment, they would steer the setup's own clients too.
	var exports []string
	for _, e := range serviceEnv(names, creds) {
		k, v, _ := strings.Cut(e, "=")
		exports = append(exports, k+`="`+v+`"`) // checked values: no $, `, " or \
	}
	o.Command = "sh -c " + shQuote(serviceSetup(o.Docker, names, creds)+" && export "+strings.Join(exports, " ")+" && "+unwrap(o.Command))
	o.Net = "allow"
	o.Allow = appendNew(o.Allow, mirrors(o.Docker)...)
	o.MemMB = max(o.MemMB, 2048)
	o.TimeoutS = max(o.TimeoutS, 900)
	o.Services = labels
	note := "Starts " + strings.Join(labels, " and ") + " inside the VM first (installed from the distribution's mirror, listening on 127.0.0.1 only)"
	if len(from) > 0 {
		note += ", with the user and database the project's own settings name"
	}
	o.Note = joinNote(o.Note, note+".")
	return o
}

func joinNote(a, b string) string {
	if a == "" {
		return b
	}
	return a + " " + b
}

func appendNew(list []string, add ...string) []string {
	out := append([]string(nil), list...)
	for _, a := range add {
		found := false
		for _, l := range out {
			found = found || l == a
		}
		if !found {
			out = append(out, a)
		}
	}
	return out
}
