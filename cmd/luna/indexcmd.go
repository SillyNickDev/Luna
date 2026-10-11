package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"luna/internal/capsule"
	"luna/internal/config"
	"luna/internal/index"
	"luna/internal/plugin"
)

// fetchIndex downloads and verifies the configured plugin index.
func fetchIndex(p paths) (*index.Index, string, error) {
	cfg, err := config.Load(p.config)
	if err != nil {
		return nil, "", err
	}
	if cfg.Index.URL == "" || cfg.Index.PublicKey == "" {
		return nil, "", errors.New("no plugin index is set up: add [index] url and public_key to luna.toml")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ix, err := index.Fetch(ctx, cfg.Index.URL, cfg.Index.PublicKey)
	return ix, cfg.Index.URL, err
}

// searchIndex is luna capsule search [term]: the newest version of each matching plugin.
func searchIndex(p paths, args []string) error {
	ix, _, err := fetchIndex(p)
	if err != nil {
		return err
	}
	term := strings.ToLower(strings.Join(args, " "))
	newest := map[string]index.Entry{}
	for _, e := range ix.Plugins {
		if ix.IsRevoked(e.ID, e.Version) != "" || !strings.Contains(strings.ToLower(e.ID+" "+e.Name), term) {
			continue
		}
		if cur, ok := newest[e.ID]; !ok || index.Newer(e.Version, cur.Version) {
			newest[e.ID] = e
		}
	}
	if len(newest) == 0 {
		fmt.Println("nothing in the plugin index matches")
		return nil
	}
	ids := make([]string, 0, len(newest))
	for id := range newest {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	for _, id := range ids {
		e := newest[id]
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.ID, e.Version, e.Runtime, e.Name)
	}
	w.Flush()
	fmt.Println("install with: luna capsule install <id>")
	return nil
}

// updateFromIndex is luna capsule update [name]: newer versions of installed plugins from the
// index (asking again if their permissions changed), and revoked ones disabled.
func updateFromIndex(p paths, args []string) error {
	ix, base, err := fetchIndex(p)
	if err != nil {
		return err
	}
	out, err := callOut(p, request{Cmd: "capsule", Args: []string{"list", "--json"}})
	if err != nil {
		return err
	}
	var installed []struct{ ID, Version, State string }
	if err := json.Unmarshal([]byte(out), &installed); err != nil {
		return err
	}
	updated := 0 // updated or disabled
	for _, c := range installed {
		if len(args) > 0 && !slices.Contains(args, c.ID) && !slices.Contains(args, strings.TrimPrefix(c.ID, "luna.")) {
			continue
		}
		if why := ix.IsRevoked(c.ID, c.Version); why != "" {
			fmt.Printf("%s %s was revoked by the index (%s); disabling it\n", c.ID, c.Version, why)
			if err := call(p, request{Cmd: "capsule", Args: []string{"disable", c.ID}}); err != nil {
				return err
			}
			updated++
			continue
		}
		e, err := ix.Find(c.ID)
		if err != nil || !index.Newer(e.Version, c.Version) {
			continue // not from the index (first-party, local), or already the newest
		}
		tmp, err := os.MkdirTemp("", "luna-dl-")
		if err != nil {
			return err
		}
		fmt.Printf("updating %s %s → %s\n", c.ID, c.Version, e.Version)
		src, err := index.Download(context.Background(), base, e, tmp)
		if err == nil {
			err = installPackage(p, src, false, e)
		}
		os.RemoveAll(tmp)
		if err != nil {
			fmt.Printf("  %s: %v\n", c.ID, err)
			continue
		}
		updated++
	}
	if updated == 0 {
		fmt.Println("everything is up to date")
	}
	return nil
}

// indexCLI is the maintainer's side: luna index keygen | sign | revoke.
func indexCLI(args []string) error {
	usage := errors.New("usage: luna index keygen <keyfile> | sign <dir> --key <keyfile> | revoke <dir> --key <keyfile> <id> <version|*> <reason>")
	if len(args) < 2 {
		return usage
	}
	switch args[0] {
	case "keygen":
		if _, err := os.Stat(args[1]); err == nil {
			return fmt.Errorf("%s exists; not overwriting a signing key", args[1])
		}
		pub, err := index.Keygen(args[1])
		if err != nil {
			return err
		}
		fmt.Printf("Private key written to %s: keep it offline and backed up; anyone with it can sign plugins.\n", args[1])
		fmt.Printf("Public key, for [index] public_key in luna.toml:\n%s\n", pub)
		return nil
	case "sign", "revoke":
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		key := fs.String("key", "", "the private key from luna index keygen")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if *key == "" {
			return usage
		}
		dir := args[1]
		ix, err := buildIndex(dir)
		if err != nil {
			return err
		}
		if args[0] == "revoke" {
			r := fs.Args()
			if len(r) < 3 {
				return usage
			}
			ix.Revoked = append(ix.Revoked, index.Revoked{ID: r[0], Version: r[1], Reason: strings.Join(r[2:], " ")})
		}
		if err := index.Sign(ix, dir, *key); err != nil {
			return err
		}
		fmt.Printf("signed %s/index.json: %d packages, %d revoked\n", dir, len(ix.Plugins), len(ix.Revoked))
		return nil
	}
	return usage
}

// buildIndex lists every .capsule file in dir, keeping the revocations already published there.
func buildIndex(dir string) (*index.Index, error) {
	ix := &index.Index{Plugins: []index.Entry{}}
	if raw, err := os.ReadFile(filepath.Join(dir, "index.json")); err == nil {
		var prev index.Index
		if err := json.Unmarshal(raw, &prev); err != nil {
			return nil, fmt.Errorf("the existing index.json: %w", err)
		}
		ix.Revoked = prev.Revoked
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.capsule"))
	store, err := os.MkdirTemp("", "luna-index-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(store)
	for _, f := range files {
		u, err := capsule.AddToStore(store, f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		fi, _ := os.Stat(f)
		e := index.Entry{ID: u.Manifest.ID, Version: u.Manifest.Version, Runtime: u.Manifest.Runtime,
			File: filepath.Base(f), Hash: u.Hash, Size: fi.Size()}
		if pm, err := plugin.Load(u.Dir); err == nil {
			e.Name = pm.Name
		}
		ix.Plugins = append(ix.Plugins, e)
	}
	return ix, nil
}
