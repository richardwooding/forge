package tool

// Level is a log severity.
type Level int32

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// Context is what a handler is given. It is the only way to reach the host, so
// a tool's dependence on the outside world is visible in its signature.
//
// It is deliberately not a context.Context. Cancellation on wasip1 does not
// work by a guest noticing a closed channel: when a deadline passes, forge
// closes the module out from under it. A handler that polled ctx.Done() would
// be writing code that never runs, which is worse than not offering it.
type Context struct {
	op string
}

// Op is the name of the operation being invoked, for a handler shared between
// several.
func (c *Context) Op() string { return c.op }

// Logf writes a line to forge's log. It never reaches the tool's own output, so
// it is safe to call from a tool whose result is piped somewhere.
func (c *Context) Logf(level Level, format string, args ...any) {
	hostLog(int32(level), sprintf(format, args...))
}

// Debugf, Infof, Warnf and Errorf are shorthands for Logf.
func (c *Context) Debugf(format string, args ...any) { c.Logf(LevelDebug, format, args...) }
func (c *Context) Infof(format string, args ...any)  { c.Logf(LevelInfo, format, args...) }
func (c *Context) Warnf(format string, args ...any)  { c.Logf(LevelWarn, format, args...) }
func (c *Context) Errorf(format string, args ...any) { c.Logf(LevelError, format, args...) }

// Progress reports how far through the work the tool is. total may be zero when
// the tool does not know. forge shows this in the CLI and REPL, and forwards it
// over MCP when the client asked for progress.
func (c *Context) Progress(done, total int64, message string) {
	hostProgress(done, total, message)
}
