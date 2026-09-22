package impact

import "errors"

var (
	errInvalidRepositoryID = errors.New("invalid repository id")
	errInvalidGraph        = errors.New("invalid impact graph")
	errRepositoryLimit     = errors.New("impact graph repository limit exceeded")
	errEdgeLimit           = errors.New("impact graph edge limit exceeded")
)
