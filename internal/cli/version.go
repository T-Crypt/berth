package cli

import (
	"runtime"

	"github.com/T-Crypt/berth/internal/proto"
)

// versionDoc is what `berth version --format json|yaml` prints: the release,
// the toolchain it was built with, and the wire version it speaks, which is
// what a bug report needs.
type versionDoc struct {
	Version  string `json:"version" yaml:"version"`
	Go       string `json:"go_version" yaml:"go_version"`
	Platform string `json:"platform" yaml:"platform"`
	API      int    `json:"api" yaml:"api"`
}

type versionView struct{ version string }

func (v versionView) Data() any {
	return versionDoc{
		Version:  v.version,
		Go:       runtime.Version(),
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
		API:      proto.APIVersion,
	}
}

func (v versionView) Human(c *Context, ok bool) error {
	c.Printf("berth %s\n", v.version)
	if c.Verbose {
		c.Bullet("go %s, %s", runtime.Version(), runtime.GOOS+"/"+runtime.GOARCH)
	}
	return nil
}

func runVersion(c *Context) int {
	fs := c.FlagSet()
	if code, done := c.Parse(fs); done {
		return code
	}
	if code, done := c.noArgs(); done {
		return code
	}
	return c.Write(versionView{version: c.Version}, nil)
}
