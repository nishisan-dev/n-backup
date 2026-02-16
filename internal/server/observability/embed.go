// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package observability

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:web
var webContent embed.FS

// WebFS retorna um http.FileSystem apontando para o conteúdo embarcado
// da pasta web/, servindo index.html como raiz.
func WebFS() http.FileSystem {
	sub, err := fs.Sub(webContent, "web")
	if err != nil {
		panic("embedded web content missing: " + err.Error())
	}
	return http.FS(sub)
}
