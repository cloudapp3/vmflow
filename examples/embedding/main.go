package main

import (
	"fmt"

	"github.com/cloudapp3/vmflow"
	"github.com/cloudapp3/vmflow/engine"
)

func main() {
	rt := vmflow.New()
	defer rt.Close()

	// Disabled so this example can build and run without binding a local port.
	rules := []engine.Rule{{
		RuleID:     "embedded-demo",
		Name:       "embedded-demo",
		Protocol:   engine.ProtocolTCP,
		ListenAddr: "127.0.0.1",
		ListenPort: 2201,
		TargetAddr: "127.0.0.1",
		TargetPort: 22,
		Enabled:    false,
	}}

	result := rt.Apply(rules)
	fmt.Printf("applied=%d stopped=%d failed=%d total=%d\n",
		result.AppliedRules,
		result.StoppedRules,
		result.FailedRules,
		result.TotalRules,
	)

	for _, snapshot := range rt.SnapshotAll() {
		fmt.Printf("%s upload=%d download=%d conns=%d\n",
			snapshot.RuleID,
			snapshot.UploadBytes,
			snapshot.DownloadBytes,
			snapshot.Conns,
		)
	}
}
