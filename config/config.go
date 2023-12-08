package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"regexp"
)

// 利用 https://mholt.github.io/json-to-go/ 生成
type Config struct {
	Server struct {
		ServerIP string `json:"ServerIP"`
		ServerPort int `json:"ServerPort"`
	} `json:"server"`
	Client struct {
		TotalPeers int   `json:"TotalPeers"`
		IPList     []any `json:"IPList"`
	} `json:"client"`
	Port struct {
		DataPort int `json:"DataPort"`
		HTTPPort int `json:"HttpPort"`
	} `json:"port"`
	Model struct {
		ModelPath string `json:"ModelPath"`
		ModelName string `json:"ModelName"`
	} `json:"model"`
	Storage struct {
		Method string `json:"Method"`
	} `json:"storage"`
	Tracker struct {
		URLList [][]string `json:"URLList"`
	} `json:"tracker"`
	TorrentLib struct {
		Debug bool `json:"Debug"`
		PieceSelectionStrategy string `json:"PieceSelectionStrategy"`
	} `json:"torrentLib"`
}

// 去除jsonc文件中的注释
func removeComments(jsonc string) string {
	commentRegex := regexp.MustCompile(`(?m)(?s)// .*?$|/\*.*?\*/`)
	tmp := commentRegex.ReplaceAllString(jsonc, "")
	re := regexp.MustCompile(`(?m)^\s*$[\r\n]*`)
	return re.ReplaceAllString(tmp, "")
}

// 读取jsonc文件并去除注释
func ReadJsonc(jsoncFileName string) (string, error) {
	var err error
	var confPath string

	// 检查是否是绝对路径
	// 不是绝对路径, 则confPath是工作目录加上传入的参数
	if path.IsAbs(jsoncFileName) {
		confPath = jsoncFileName
	} else {
		currentPath, _ := os.Getwd()
		confPath = path.Join(currentPath, jsoncFileName)
	}

	_, err = os.Stat(confPath)
	if err != nil {
		return "", errors.New(fmt.Sprintf("config file is not found %s", confPath))
	}

	file, _ := os.Open(confPath)
	defer file.Close()

	jsoncData, err := ioutil.ReadAll(file)
	if err != nil {
		return "", errors.New(fmt.Sprintf("read config file error %s", confPath))
	}

	jsonData := removeComments(string(jsoncData))
	return jsonData, nil
}

// 加载jsonc文件
func LoadJsonc(jsoncFileName string) (*Config, error) {
	jsonData, err := ReadJsonc(jsoncFileName)
	if err != nil {
		return nil, err
	}

	config := Config{}
	if err := json.Unmarshal([]byte(jsonData), &config); err != nil {
		return nil, errors.New(fmt.Sprintf("json unmarshal error %s", err))
	}
	return &config, nil
}
