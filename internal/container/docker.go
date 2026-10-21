package container

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Docker implements Runtime with the docker CLI (Docker Desktop, colima, …).
type Docker struct{}

// docker runs a short CLI call. Long-running ones (logs -f, wait) use stream.
func (Docker) docker(ctx context.Context, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute) // `run` may pull the image
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func (d Docker) Available(ctx context.Context) error {
	_, err := d.docker(ctx, nil, "info", "--format", "{{.ServerVersion}}")
	return err
}

func (d Docker) Start(ctx context.Context, s Spec) (string, error) {
	args, env, err := runArgs(s)
	if err != nil {
		return "", err
	}
	return d.docker(ctx, env, args...)
}

// runArgs builds `docker run`. Env values travel in the CLI's own environment
// (`-e NAME` copies it), so they never show up in argv.
func runArgs(s Spec) (args, env []string, err error) {
	if s.Image == "" || len(s.Command) == 0 {
		return nil, nil, fmt.Errorf("container spec needs an image and a command")
	}
	args = []string{"run", "--detach", "--init"}
	if s.Name != "" {
		args = append(args, "--name", s.Name)
	}
	for _, k := range slices.Sorted(maps.Keys(s.Labels)) {
		args = append(args, "--label", k+"="+s.Labels[k])
	}
	for _, k := range slices.Sorted(maps.Keys(s.Env)) {
		args = append(args, "--env", k)
		env = append(env, k+"="+s.Env[k])
	}
	if s.EnvFile != "" {
		args = append(args, "--env-file", s.EnvFile)
	}
	for _, m := range s.Mounts {
		if strings.ContainsAny(m.Source+m.Target, ":,") {
			return nil, nil, fmt.Errorf("mount path %q contains ':' or ','", m.Source)
		}
		v := m.Source + ":" + m.Target
		if m.ReadOnly {
			v += ":ro"
		}
		args = append(args, "--volume", v)
	}
	if s.Workdir != "" {
		args = append(args, "--workdir", s.Workdir)
	}
	args = append(args, "--entrypoint", s.Command[0], s.Image)
	return append(args, s.Command[1:]...), env, nil
}

func (Docker) Logs(ctx context.Context, id string, w io.Writer) error {
	cmd := exec.CommandContext(ctx, "docker", "logs", "--follow", id)
	cmd.Stdout, cmd.Stderr = w, w
	return cmd.Run()
}

func (Docker) Wait(ctx context.Context, id string) (int, error) {
	out, err := exec.CommandContext(ctx, "docker", "wait", id).Output()
	if err != nil {
		return 0, fmt.Errorf("docker wait %s: %w", id, err)
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

func (d Docker) Stop(ctx context.Context, id string) error {
	_, err := d.docker(ctx, nil, "stop", "--time", "10", id)
	return err
}

func (d Docker) Remove(ctx context.Context, id string) error {
	_, err := d.docker(ctx, nil, "rm", "--force", id)
	return err
}

func (d Docker) List(ctx context.Context, labels map[string]string) ([]Info, error) {
	args := []string{"ps", "--all", "--quiet", "--no-trunc"}
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		args = append(args, "--filter", "label="+k+"="+labels[k])
	}
	ids, err := d.docker(ctx, nil, args...)
	if err != nil || ids == "" {
		return nil, err
	}
	out, err := d.docker(ctx, nil, append([]string{"inspect"}, strings.Fields(ids)...)...)
	if err != nil {
		return nil, err
	}
	return parseInspect([]byte(out))
}

func parseInspect(b []byte) ([]Info, error) {
	var rows []struct {
		ID     string `json:"Id"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		State struct {
			Running  bool `json:"Running"`
			ExitCode int  `json:"ExitCode"`
		} `json:"State"`
	}
	if err := json.Unmarshal(b, &rows); err != nil {
		return nil, fmt.Errorf("parse docker inspect: %w", err)
	}
	infos := make([]Info, len(rows))
	for i, r := range rows {
		infos[i] = Info{ID: r.ID, Labels: r.Config.Labels, Running: r.State.Running, ExitCode: r.State.ExitCode}
	}
	return infos, nil
}
