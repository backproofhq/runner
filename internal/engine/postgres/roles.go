package postgres

import (
	"bytes"
	"regexp"
	"slices"
	"strings"
)

const (
	roleIdentifier = `"(?:[^"]|"")+"|[A-Za-z_][A-Za-z0-9_$]*`
	roleList       = `((?:` + roleIdentifier + `)(?:\s*,\s*(?:` + roleIdentifier + `))*)`

	// A dump full of generated role names must not grow the script without limit.
	maxRoles = 10000
)

var (
	roleName     = regexp.MustCompile(roleIdentifier)
	rolePatterns = []*regexp.Regexp{
		regexp.MustCompile(`^ALTER .+ OWNER TO ` + roleList + `;`),
		regexp.MustCompile(`^GRANT .+? TO ` + roleList),
		regexp.MustCompile(`^REVOKE .+? FROM ` + roleList),
		regexp.MustCompile(`^CREATE POLICY .+? TO ` + roleList),
		regexp.MustCompile(`^ALTER DEFAULT PRIVILEGES FOR ROLE ` + roleList),
	}
	sessionRole     = regexp.MustCompile(`^SET SESSION AUTHORIZATION '([^']+)'`)
	rolePrefixes    = []string{"ALTER ", "GRANT ", "REVOKE ", "CREATE POLICY ", "SET SESSION AUTHORIZATION "}
	reservedRoles   = []string{"public", "current_user", "current_role", "session_user", superuser}
	roleListKeyword = []string{"with", "granted", "using", "restrict", "cascade", "check"}
)

// roleScanner collects the roles a dump's SQL refers to.
type roleScanner struct {
	roles map[string]struct{}
}

func newRoleScanner() *roleScanner {
	return &roleScanner{roles: make(map[string]struct{})}
}

func (s *roleScanner) observe(line string) {
	if !slices.ContainsFunc(rolePrefixes, func(prefix string) bool { return strings.HasPrefix(line, prefix) }) {
		return
	}
	if match := sessionRole.FindStringSubmatch(line); match != nil {
		s.add(match[1])
		return
	}
	for _, pattern := range rolePatterns {
		match := pattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		for _, token := range roleName.FindAllString(match[1], -1) {
			if name, ok := unquoteRole(token); ok {
				s.add(name)
			}
		}
		return
	}
}

func (s *roleScanner) add(name string) {
	if len(s.roles) >= maxRoles || slices.Contains(reservedRoles, name) || strings.HasPrefix(name, "pg_") {
		return
	}
	s.roles[name] = struct{}{}
}

func unquoteRole(token string) (string, bool) {
	if strings.HasPrefix(token, `"`) {
		return strings.ReplaceAll(token[1:len(token)-1], `""`, `"`), true
	}
	name := strings.ToLower(token)
	return name, !slices.Contains(roleListKeyword, name)
}

// script returns the SQL that creates the collected roles, sorted for repeatable runs.
func (s *roleScanner) script() []byte {
	names := make([]string, 0, len(s.roles))
	for name := range s.roles {
		names = append(names, name)
	}
	slices.Sort(names)

	var script bytes.Buffer
	for _, name := range names {
		script.WriteString(`CREATE ROLE "` + strings.ReplaceAll(name, `"`, `""`) + `";` + "\n")
	}
	return script.Bytes()
}
