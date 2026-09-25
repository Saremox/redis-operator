package utils

import (
	"flag"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestToRedisOperatorConfigDisconnectClientsOnDemotion(t *testing.T) {
	enabled := (&CMDFlags{DisconnectClientsOnDemotion: true}).ToRedisOperatorConfig()
	assert.False(t, enabled.KeepClientsOnDemotion)

	disabled := (&CMDFlags{DisconnectClientsOnDemotion: false}).ToRedisOperatorConfig()
	assert.True(t, disabled.KeepClientsOnDemotion)
}

func TestInitDisconnectClientsOnDemotion(t *testing.T) {
	tests := map[string]struct {
		args []string
		want bool
	}{
		"on by default": {want: true},
		"turned off":    {args: []string{"--disconnect-clients-on-demotion=false"}, want: false},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			defer func(args []string, commandLine *flag.FlagSet) {
				os.Args, flag.CommandLine = args, commandLine
			}(os.Args, flag.CommandLine)
			os.Args = append([]string{"redis-operator"}, test.args...)
			flag.CommandLine = flag.NewFlagSet("redis-operator", flag.ContinueOnError)

			var flags CMDFlags
			flags.Init()

			assert.Equal(t, test.want, flags.DisconnectClientsOnDemotion)
		})
	}
}
