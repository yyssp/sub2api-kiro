package resources

import _ "embed"

//go:embed model-pricing/model_prices_and_context_window.json
var ModelCatalogJSON []byte
