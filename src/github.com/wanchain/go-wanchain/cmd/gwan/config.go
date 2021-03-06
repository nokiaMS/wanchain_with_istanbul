// Copyright 2017 The go-ethereum Authors
// This file is part of go-ethereum.
//
// go-ethereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-ethereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-ethereum. If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"unicode"

	cli "gopkg.in/urfave/cli.v1"

	"github.com/naoina/toml"
	"github.com/wanchain/go-wanchain/cmd/utils"
	"github.com/wanchain/go-wanchain/contracts/release"
	"github.com/wanchain/go-wanchain/eth"
	"github.com/wanchain/go-wanchain/node"
	"github.com/wanchain/go-wanchain/params"
	whisper "github.com/wanchain/go-wanchain/whisper/whisperv5"
)

var (
	dumpConfigCommand = cli.Command{
		Action:      utils.MigrateFlags(dumpConfig),
		Name:        "dumpconfig",
		Usage:       "Show configuration values",
		ArgsUsage:   "",
		Flags:       append(append(nodeFlags, rpcFlags...), whisperFlags...),
		Category:    "MISCELLANEOUS COMMANDS",
		Description: `The dumpconfig command shows configuration values.`,
	}

	//可以在命令行通过config参数指定TOML格式的配置文件。
	configFileFlag = cli.StringFlag{
		Name:  "config",
		Usage: "TOML configuration file",
	}
)

// These settings ensure that TOML keys use the same names as Go struct fields.
var tomlSettings = toml.Config{
	NormFieldName: func(rt reflect.Type, key string) string {
		return key
	},
	FieldToKey: func(rt reflect.Type, field string) string {
		return field
	},
	MissingField: func(rt reflect.Type, field string) error {
		link := ""
		if unicode.IsUpper(rune(rt.Name()[0])) && rt.PkgPath() != "main" {
			link = fmt.Sprintf(", see https://godoc.org/%s#%s for available fields", rt.PkgPath(), rt.Name())
		}
		return fmt.Errorf("field '%s' is not defined in %s%s", field, rt.String(), link)
	},
}

type ethstatsConfig struct {
	URL string `toml:",omitempty"`
}

//geth config对象。
type gethConfig struct {
	Eth      eth.Config	//eth配置。
	Shh      whisper.Config	//whisper配置。
	Node     node.Config	//node配置属性。
	Ethstats ethstatsConfig	//ethstats配置。
}

//从配置文件加载配置信息到cfg中。
func loadConfig(file string, cfg *gethConfig) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()

	err = tomlSettings.NewDecoder(bufio.NewReader(f)).Decode(cfg)
	// Add file name to errors that have a line number.
	if _, ok := err.(*toml.LineError); ok {
		err = errors.New(file + ", " + err.Error())
	}
	return err
}

//node默认配置。
func defaultNodeConfig() node.Config {
	cfg := node.DefaultConfig
	cfg.Name = clientIdentifier
	cfg.Version = params.VersionWithCommit(gitCommit)
	cfg.HTTPModules = append(cfg.HTTPModules, "eth", "shh")
	cfg.HTTPModules = append(cfg.HTTPModules, "wan", "shh")
	cfg.WSModules = append(cfg.WSModules, "eth", "shh")
	cfg.WSModules = append(cfg.WSModules, "wan", "shh")
	cfg.IPCPath = "gwan.ipc"
	return cfg
}

func makeConfigNode(ctx *cli.Context) (*node.Node, gethConfig) {
	// Load defaults.
	cfg := gethConfig{	//加载默认配置。
		Eth:  eth.DefaultConfig,	//如果命令行参数设置了对应的参数值,那么此处的eth.DefaultConfig中的值到这一步骤已经被修改成了命令行参数中传递过来的值.
		Shh:  whisper.DefaultConfig,
		Node: defaultNodeConfig(),
	}

	// Load config file.	//从配置文件加载配置信息。
	if file := ctx.GlobalString(configFileFlag.Name); file != "" {	//获得配置文件名称。
		if err := loadConfig(file, &cfg); err != nil {	//从配置文件加载配置信息。
			utils.Fatalf("%v", err)
		}
	}

	// Apply flags.
	utils.SetNodeConfig(ctx, &cfg.Node)	//构造node配置属性。
	stack, err := node.New(&cfg.Node)	//生成一个p2p node。
	if err != nil {
		utils.Fatalf("Failed to create the protocol stack: %v", err)
	}
	utils.SetEthConfig(ctx, stack, &cfg.Eth)
	if ctx.GlobalIsSet(utils.EthStatsURLFlag.Name) {
		cfg.Ethstats.URL = ctx.GlobalString(utils.EthStatsURLFlag.Name)
	}

	utils.SetShhConfig(ctx, stack, &cfg.Shh)

	return stack, cfg
}

// enableWhisper returns true in case one of the whisper flags is set.
func enableWhisper(ctx *cli.Context) bool {
	for _, flag := range whisperFlags {
		if ctx.GlobalIsSet(flag.GetName()) {
			return true
		}
	}
	return false
}

//创建一个full node。
func makeFullNode(ctx *cli.Context) *node.Node {
	stack, cfg := makeConfigNode(ctx)	//返回一个p2p node对象和一个cfg对象.

	utils.RegisterEthService(stack, &cfg.Eth)	//注册ethereum服务到p2p node上.

	// Whisper must be explicitly enabled by specifying at least 1 whisper flag or in dev mode
	shhEnabled := enableWhisper(ctx)
	shhAutoEnabled := !ctx.GlobalIsSet(utils.WhisperEnabledFlag.Name) && ctx.GlobalIsSet(utils.DevModeFlag.Name)
	if shhEnabled || shhAutoEnabled {
		if ctx.GlobalIsSet(utils.WhisperMaxMessageSizeFlag.Name) {
			cfg.Shh.MaxMessageSize = uint32(ctx.Int(utils.WhisperMaxMessageSizeFlag.Name))
		}
		if ctx.GlobalIsSet(utils.WhisperMinPOWFlag.Name) {
			cfg.Shh.MinimumAcceptedPOW = ctx.Float64(utils.WhisperMinPOWFlag.Name)
		}
		utils.RegisterShhService(stack, &cfg.Shh)
	}

	// Add the Ethereum Stats daemon if requested.
	if cfg.Ethstats.URL != "" {
		utils.RegisterEthStatsService(stack, cfg.Ethstats.URL)
	}

	// Add the release oracle service so it boots along with node. //注册版本更新提示服务.
	if err := stack.Register(func(ctx *node.ServiceContext) (node.Service, error) {
		config := release.Config{
			Oracle: relOracle,
			Major:  uint32(params.VersionMajor),
			Minor:  uint32(params.VersionMinor),
			Patch:  uint32(params.VersionPatch),
		}
		commit, _ := hex.DecodeString(gitCommit)
		copy(config.Commit[:], commit)
		return release.NewReleaseService(ctx, config)
	}); err != nil {
		utils.Fatalf("Failed to register the Geth release oracle service: %v", err)
	}
	return stack
}

// dumpConfig is the dumpconfig command.
func dumpConfig(ctx *cli.Context) error {
	_, cfg := makeConfigNode(ctx)
	comment := ""

	if cfg.Eth.Genesis != nil {
		cfg.Eth.Genesis = nil
		comment += "# Note: this config doesn't contain the genesis block.\n\n"
	}

	out, err := tomlSettings.Marshal(&cfg)
	if err != nil {
		return err
	}
	io.WriteString(os.Stdout, comment)
	os.Stdout.Write(out)
	return nil
}
