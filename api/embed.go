// Пакет api хранит единую спецификацию публичного HTTP API.
package api

import _ "embed"

//go:embed openapi.yaml
var document string

// OpenAPI возвращает встроенную спецификацию без изменений.
func OpenAPI() string { return document }
