package cli

import "github.com/T-Crypt/berth/internal/apikey"

func runKey(c *Context) int {
	fs := c.FlagSet()
	file := fs.String("file", apikey.DefaultPath(), "the key file ($BERTH_API_KEY_FILE)")
	if code, done := c.Parse(fs); done {
		return code
	}
	if code, done := c.noArgs(); done {
		return code
	}
	key, err := apikey.LoadOrCreate(*file)
	if err != nil {
		return c.fail(Systemf("berth key: %v", err))
	}
	c.Printf("%s\n", key)
	return ExitOK
}
