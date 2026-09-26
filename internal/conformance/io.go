package conformance

import (
	"io"
	"net/http"
)

func readAll(res *http.Response) ([]byte, error) { return io.ReadAll(res.Body) }
