package core

// Price: USD per million tokens of each kind; CacheCreate is a 1-hour cache write (2× input), the one Claude Code
// makes: its reported cost matches it.
type Price struct {
	Input       float64 `json:"input"`
	Output      float64 `json:"output"`
	CacheRead   float64 `json:"cacheRead"`
	CacheCreate float64 `json:"cacheCreate"`
}

// Prices: by model family (ModelFamily).
type Prices map[string]Price

// DefaultPrices: the list prices of the current model of each family; the config can change them.
func DefaultPrices() Prices {
	return Prices{
		"opus":   {Input: 4, Output: 20, CacheRead: 0.2, CacheCreate: 8},
		"sonnet": {Input: 2, Output: 10, CacheRead: 0.2, CacheCreate: 4},
		"haiku":  {Input: 0.1, Output: 0.5, CacheRead: 0.01, CacheCreate: 0.2},
		"fable":  {Input: 10, Output: 50, CacheRead: 0.25, CacheCreate: 20},
	}
}

// Cost: what the tokens cost on the model at its list price; 0 for a model with no price.
func (p Prices) Cost(model string, u Usage) float64 {
	pr, ok := p[ModelFamily(model)]
	if !ok {
		return 0
	}
	return (float64(u.Input)*pr.Input + float64(u.Output)*pr.Output + float64(u.CacheRead)*pr.CacheRead + float64(u.CacheCreate)*pr.CacheCreate) / 1e6
}
