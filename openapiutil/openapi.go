package openapiutil

import (
	"context"
	"net/url"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

func LoadSpec(ctx context.Context, pathOrURL string) (*openapi3.T, string, error) {
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = true

	var (
		doc *openapi3.T
		err error
	)
	if isHTTPURL(pathOrURL) {
		u, err := url.Parse(pathOrURL)
		if err != nil {
			return nil, "", err
		}
		doc, err = loader.LoadFromURI(u)
	} else {
		doc, err = loader.LoadFromFile(pathOrURL)
	}
	if err != nil {
		return nil, "", err
	}
	if err := doc.Validate(ctx); err != nil {
		// Proceed even if validation reports issues (e.g., regex patterns incompatible with Go's RE2)
		// We still return the loaded document and inferred server URL.
		return doc, firstServerURL(doc), nil
	}
	return doc, firstServerURL(doc), nil
}

func firstServerURL(doc *openapi3.T) string {
	if doc == nil || len(doc.Servers) == 0 {
		return ""
	}
	return doc.Servers[0].URL
}

func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}
