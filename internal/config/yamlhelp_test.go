package config

import "gopkg.in/yaml.v3"

func unmarshalYAML(b []byte, out any) error { return yaml.Unmarshal(b, out) }
