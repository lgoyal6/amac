//go:build !darwin && !linux

package supervisor

import "fmt"

func writeExactPath(string, []byte) error {
	return fmt.Errorf("exact-path writes require Darwin or Linux no-follow filesystem support")
}
