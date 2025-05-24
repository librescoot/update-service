package config

// Config holds the application configuration
type Config struct {
	Redis RedisConfig
}

// RedisConfig holds Redis connection configuration
type RedisConfig struct {
	Addr     string
	Password string
	DB       int
}

