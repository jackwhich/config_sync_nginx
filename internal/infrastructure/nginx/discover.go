package nginx

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"nginx_updata_config/internal/config"
	"nginx_updata_config/internal/infrastructure/process"
)

// Discover reuses the running master and its launch flags. It never starts nginx
// or edits nginx.conf. Explicit complete settings retain the legacy PID-file mode.
func Discover(ctx context.Context, override config.Nginx) (config.Nginx, error) {
	if override.Binary != "" && override.ConfigFile != "" && override.PIDFile != "" {
		return override, nil
	}
	out, err := process.Run(ctx, "", nil, nil, "ps", "-ww", "-eo", "pid=,ppid=,args=")
	if err != nil {
		return config.Nginx{}, err
	}
	wantedPID := 0
	if override.PIDFile != "" {
		b, err := os.ReadFile(override.PIDFile)
		if err != nil {
			return config.Nginx{}, err
		}
		wantedPID, err = strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil || wantedPID <= 1 {
			return config.Nginx{}, fmt.Errorf("invalid nginx pid file")
		}
	}
	var matches []config.Nginx
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 || strings.Join(fields[2:5], " ") != "nginx: master process" {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 1 {
			continue
		}
		if wantedPID != 0 && wantedPID != pid {
			continue
		}
		title := strings.TrimSpace(strings.SplitN(line, "nginx: master process", 2)[1])
		found, err := parseMaster(title)
		if err != nil {
			return config.Nginx{}, fmt.Errorf("nginx master %d: %w", pid, err)
		}
		if binary, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
			found.Binary = binary
		}
		if !filepath.IsAbs(found.Binary) {
			binary, err := exec.LookPath(found.Binary)
			if err != nil {
				return config.Nginx{}, err
			}
			found.Binary = binary
		}
		if override.Binary != "" && !sameFile(override.Binary, found.Binary) {
			continue
		}
		if override.ConfigFile != "" && found.ConfigFile != "" && override.ConfigFile != found.ConfigFile {
			continue
		}
		if override.Prefix != "" && found.Prefix != "" && override.Prefix != found.Prefix {
			continue
		}
		if override.Binary != "" {
			found.Binary = override.Binary
		}
		if override.ConfigFile != "" {
			found.ConfigFile = override.ConfigFile
		}
		if override.Prefix != "" {
			found.Prefix = override.Prefix
		}
		// Do not guess relative launch paths when the prefix is unknown.
		if (found.Prefix != "" && !filepath.IsAbs(found.Prefix)) || (found.ConfigFile != "" && !filepath.IsAbs(found.ConfigFile) && found.Prefix == "") {
			return config.Nginx{}, fmt.Errorf("relative nginx launch paths require optional absolute nginx.prefix/config_file overrides")
		}
		found.MasterPID = pid
		matches = append(matches, found)
	}
	if len(matches) != 1 {
		return config.Nginx{}, fmt.Errorf("expected one running nginx master, found %d; for multiple instances set optional nginx.pid_file", len(matches))
	}
	return matches[0], nil
}

func sameFile(a, b string) bool {
	left, e1 := os.Stat(a)
	right, e2 := os.Stat(b)
	return e1 == nil && e2 == nil && os.SameFile(left, right)
}

func parseMaster(title string) (config.Nginx, error) {
	args, err := splitArguments(title)
	if err != nil || len(args) == 0 {
		return config.Nginx{}, fmt.Errorf("cannot parse nginx launch arguments")
	}
	n := config.Nginx{Binary: args[0]}
	for i := 1; i < len(args); i++ {
		flag := args[i]
		if flag == "-q" {
			continue
		}
		if len(flag) < 2 || !strings.Contains("cpeg", flag[1:2]) || flag[0] != '-' {
			return n, fmt.Errorf("unsupported nginx launch argument %q", flag)
		}
		value := flag[2:]
		if value == "" {
			i++
			if i >= len(args) {
				return n, fmt.Errorf("missing nginx option value")
			}
			value = args[i]
		}
		if flag[1] == 'g' {
			// nginx's process title joins argv with spaces and may drop -g quoting.
			for i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				value += " " + args[i]
			}
		}
		switch flag[1] {
		case 'c':
			n.ConfigFile = value
		case 'p':
			n.Prefix = value
		case 'e':
			n.ErrorLog = value
		case 'g':
			n.GlobalDirectives = value
		}
	}
	return n, nil
}

func splitArguments(s string) ([]string, error) {
	var args []string
	var token strings.Builder
	var quote rune
	escaped := false
	for _, c := range s {
		if escaped {
			token.WriteRune(c)
			escaped = false
			continue
		}
		if c == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			} else {
				token.WriteRune(c)
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		if c == ' ' || c == '\t' {
			if token.Len() > 0 {
				args = append(args, token.String())
				token.Reset()
			}
			continue
		}
		token.WriteRune(c)
	}
	if escaped || quote != 0 {
		return nil, fmt.Errorf("unterminated nginx launch argument")
	}
	if token.Len() > 0 {
		args = append(args, token.String())
	}
	return args, nil
}
