package nginx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nginx_updata_config/internal/config"
)

func TestDiscoverExistingMasterWithoutNginxConfig(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "nginx")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ps := filepath.Join(root, "ps")
	writePS := func(lines string) {
		if err := os.WriteFile(ps, []byte("#!/bin/sh\ncat <<'PROCESSES'\n"+lines+"\nPROCESSES\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	title := "999991 1 nginx: master process " + binary + " -p /srv/nginx/ -c conf/nginx.conf -g daemon on; master_process on;"
	writePS(title)
	n, err := Discover(context.Background(), config.Nginx{})
	if err != nil {
		t.Fatal(err)
	}
	if n.MasterPID != 999991 || n.Binary != binary || n.ConfigFile != "conf/nginx.conf" || n.Prefix != "/srv/nginx/" || n.GlobalDirectives != "daemon on; master_process on;" {
		t.Fatalf("%+v", n)
	}
	rt := Runtime{Config: config.Config{Nginx: n}}
	if got := strings.Join(rt.args("-t"), "|"); got != "-c|conf/nginx.conf|-p|/srv/nginx/|-g|daemon on; master_process on;|-t" {
		t.Fatal(got)
	}
	writePS(title + "\n" + strings.Replace(title, "999991", "999992", 1))
	if _, err := Discover(context.Background(), config.Nginx{}); err == nil {
		t.Fatal("ambiguous masters accepted")
	}
	pidFile := filepath.Join(root, "nginx.pid")
	if err := os.WriteFile(pidFile, []byte("999992"), 0600); err != nil {
		t.Fatal(err)
	}
	if n, err := Discover(context.Background(), config.Nginx{PIDFile: pidFile}); err != nil || n.MasterPID != 999992 {
		t.Fatal(n, err)
	}
	writePS("")
	if _, err := Discover(context.Background(), config.Nginx{}); err == nil {
		t.Fatal("missing nginx accepted")
	}
}

func TestParseMasterQuotedGlobalOptions(t *testing.T) {
	for _, title := range []string{"/usr/sbin/nginx -g 'daemon on; master_process on;'", "/usr/sbin/nginx -g daemon on; master_process on;"} {
		n, err := parseMaster(title)
		if err != nil || n.GlobalDirectives != "daemon on; master_process on;" {
			t.Fatal(n, err)
		}
	}
}
