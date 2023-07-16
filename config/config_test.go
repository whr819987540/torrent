package config

import (
	"encoding/json"
	"testing"
)

func TestReadJsonc(t *testing.T) {
	jsoncFileName := "../config.jsonc"
	jsonData, err := ReadJsonc(jsoncFileName)
	t.Logf("json data: %+v", jsonData)
	if err != nil {
		t.Errorf("%v", err)
	}

	config := Config{}
	if err := json.Unmarshal([]byte(jsonData), &config); err != nil {
		t.Errorf("json unmarshal error %s", err)
	}
	t.Logf("config: %+v", config)
}
