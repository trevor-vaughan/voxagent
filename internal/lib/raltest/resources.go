package raltest

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/voxagent/voxagent/internal/catalog"
)

func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func Res(typ, title string, params map[string]any) catalog.Resource {
	return catalog.Resource{Type: typ, Title: title, Parameters: params}
}

func AugRes(typ, title string, params map[string]any, marked ...string) catalog.Resource {
	return catalog.Resource{Type: typ, Title: title, Parameters: params, SensitiveParameters: marked}
}

func ExecRes(title string, params map[string]any) catalog.Resource {
	return catalog.Resource{Type: "Exec", Title: title, Parameters: params}
}

func FileRes(path string, params map[string]any) catalog.Resource {
	p := map[string]any{}
	for k, v := range params {
		p[k] = v
	}
	return catalog.Resource{Type: "File", Title: path, Parameters: p}
}
