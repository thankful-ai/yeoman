package proxy

import "github.com/thankful-ai/yeoman/internal/yeoman"

type Config struct {
	Log                yeoman.LogConfig          `json:"log"`
	HTTP               HTTPConfig                `json:"http"`
	HTTPS              HTTPSConfig               `json:"https"`
	ProviderRegistries []yeoman.ProviderRegistry `json:"providerRegistries"`
	Subnets            []string                  `json:"subnets"`
}

type HTTPConfig struct {
	Port int `json:"port"`
}

type HTTPSConfig struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	CertBucket string `json:"certBucket"`
}
