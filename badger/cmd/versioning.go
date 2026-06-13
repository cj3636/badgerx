package cmd

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/badger/v4/versioning"
	"github.com/spf13/cobra"
)

var vcsReadOnly bool
var vcsOutput string
var vcsValueOutput string
var vcsLimit int
var vcsSince uint64
var vcsYes bool
var vcsAuditDir string
var vcsRemote string
var vcsBranch string

func init() {
	for _, c := range []*cobra.Command{versionCmd, auditCmd, gitCmd, softServeCmd} {
		c.PersistentFlags().BoolVar(&vcsReadOnly, "read-only", false, "open the versioning layer in read-only mode")
		c.PersistentFlags().StringVar(&vcsOutput, "output", "human", "output format: human|json")
		c.PersistentFlags().IntVar(&vcsLimit, "limit", 100, "maximum records to print")
		c.PersistentFlags().BoolVar(&vcsYes, "yes", false, "confirm destructive operations")
	}
	versionCmd.PersistentFlags().StringVar(&vcsValueOutput, "value-output", "text", "value output: text|raw|hex|base64|json")
	auditOperationsCmd.Flags().Uint64Var(&vcsSince, "since", 0, "only show operations after logical clock")
	gitCmd.PersistentFlags().StringVar(&vcsAuditDir, "audit-dir", "", "audit Git worktree path")
	gitPushCmd.Flags().StringVar(&vcsRemote, "remote", "", "Git remote name")
	gitPushCmd.Flags().StringVar(&vcsBranch, "branch", "", "Git branch")
	softServeCmd.PersistentFlags().StringVar(&vcsAuditDir, "audit-dir", "", "audit Git worktree path")
	softServeRemoteSetCmd.Flags().StringVar(&vcsRemote, "remote", "origin", "Git remote name")
	softServeRemoteTestCmd.Flags().StringVar(&vcsRemote, "ssh-url", "", "Soft Serve SSH URL")

	versionCmd.AddCommand(versionPutCmd, versionGetCmd, versionDeleteCmd, versionHistoryCmd, versionShowCmd, versionRollbackCmd)
	auditCmd.AddCommand(auditOperationsCmd, auditOperationCmd, auditPendingCmd, auditFailedCmd, auditRetryCmd)
	gitCmd.AddCommand(gitStatusCmd, gitPushCmd)
	softServeRemoteCmd.AddCommand(softServeRemoteSetCmd, softServeRemoteTestCmd)
	softServeCmd.AddCommand(softServeRemoteCmd)
	RootCmd.AddCommand(versionCmd, auditCmd, gitCmd, softServeCmd)
}

var versionCmd = &cobra.Command{Use: "version", Short: "Versioned key-value operations"}
var versionPutCmd = &cobra.Command{Use: "put KEY VALUE", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
	db, close, err := openVersionDB(false)
	if err != nil {
		return err
	}
	defer close()
	rec, err := db.Put(args[0], []byte(args[1]))
	if err != nil {
		return err
	}
	return printAny(rec)
}}
var versionGetCmd = &cobra.Command{Use: "get KEY", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
	db, close, err := openVersionDB(true)
	if err != nil {
		return err
	}
	defer close()
	val, rec, err := db.Get(args[0])
	if err != nil {
		return err
	}
	if vcsValueOutput == "raw" {
		_, err = os.Stdout.Write(val)
		return err
	}
	if vcsOutput == "json" {
		return printAny(map[string]any{"record": rec, "value": encodeValue(val)})
	}
	return printValue(val)
}}
var versionDeleteCmd = &cobra.Command{Use: "delete KEY", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
	if !vcsYes {
		return errors.New("delete requires --yes in non-interactive mode")
	}
	db, close, err := openVersionDB(false)
	if err != nil {
		return err
	}
	defer close()
	rec, err := db.Delete(args[0])
	if err != nil {
		return err
	}
	return printAny(rec)
}}
var versionHistoryCmd = &cobra.Command{Use: "history KEY", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
	db, close, err := openVersionDB(true)
	if err != nil {
		return err
	}
	defer close()
	hist, err := db.History(args[0])
	if err != nil {
		return err
	}
	return printAny(hist)
}}
var versionShowCmd = &cobra.Command{Use: "show KEY VERSION_ID", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
	db, close, err := openVersionDB(true)
	if err != nil {
		return err
	}
	defer close()
	rec, err := db.GetVersion(args[0], args[1])
	if err != nil {
		return err
	}
	return printAny(rec)
}}
var versionRollbackCmd = &cobra.Command{Use: "rollback KEY VERSION_ID", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
	if !vcsYes {
		return errors.New("rollback requires --yes in non-interactive mode")
	}
	db, close, err := openVersionDB(false)
	if err != nil {
		return err
	}
	defer close()
	rec, err := db.Rollback(args[0], args[1])
	if err != nil {
		return err
	}
	return printAny(rec)
}}

var auditCmd = &cobra.Command{Use: "audit", Short: "Inspect versioning audit logs"}
var auditOperationsCmd = &cobra.Command{Use: "operations", RunE: func(cmd *cobra.Command, args []string) error {
	db, close, err := openVersionDB(true)
	if err != nil {
		return err
	}
	defer close()
	ops, err := db.ListOperations(versioning.OperationListOptions{Since: vcsSince, Limit: vcsLimit})
	if err != nil {
		return err
	}
	return printAny(ops)
}}
var auditOperationCmd = &cobra.Command{Use: "operation OPERATION_ID", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
	db, close, err := openVersionDB(true)
	if err != nil {
		return err
	}
	defer close()
	op, err := db.GetOperation(args[0])
	if err != nil {
		return err
	}
	return printAny(op)
}}
var auditPendingCmd = &cobra.Command{Use: "pending", RunE: func(cmd *cobra.Command, args []string) error {
	db, close, err := openVersionDB(true)
	if err != nil {
		return err
	}
	defer close()
	ops, err := db.ListExportPending(vcsLimit)
	if err != nil {
		return err
	}
	return printAny(ops)
}}
var auditFailedCmd = &cobra.Command{Use: "failed", RunE: func(cmd *cobra.Command, args []string) error {
	db, close, err := openVersionDB(true)
	if err != nil {
		return err
	}
	defer close()
	ops, err := db.ListExportFailed(vcsLimit)
	if err != nil {
		return err
	}
	return printAny(ops)
}}
var auditRetryCmd = &cobra.Command{Use: "retry OPERATION_ID", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
	db, close, err := openVersionDB(false)
	if err != nil {
		return err
	}
	defer close()
	return db.RetryExport(args[0])
}}

var gitCmd = &cobra.Command{Use: "git", Short: "Inspect or push optional Git audit mirrors"}
var gitStatusCmd = &cobra.Command{Use: "status", RunE: func(cmd *cobra.Command, args []string) error {
	db, close, err := openVersionDB(true)
	if err != nil {
		return err
	}
	defer close()
	pending, _ := db.ListExportPending(0)
	st, err := gitExporter().GitStatus(len(pending))
	if err != nil {
		return err
	}
	return printAny(st)
}}
var gitPushCmd = &cobra.Command{Use: "push", RunE: func(cmd *cobra.Command, args []string) error { return gitExporter().GitPush(vcsRemote, vcsBranch) }}

var softServeCmd = &cobra.Command{Use: "softserve", Short: "Configure Soft Serve audit mirror remotes"}
var softServeRemoteCmd = &cobra.Command{Use: "remote"}
var softServeRemoteSetCmd = &cobra.Command{Use: "set SSH_URL", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
	e := versioning.SoftServeExporter{GitExporter: gitExporter(), Remote: args[0]}
	return e.ConfigureRemote(vcsRemote)
}}
var softServeRemoteTestCmd = &cobra.Command{Use: "test", RunE: func(cmd *cobra.Command, args []string) error {
	e := versioning.SoftServeExporter{GitExporter: gitExporter(), Remote: vcsRemote}
	return e.TestRemote()
}}

func openVersionDB(readOnlyOK bool) (*versioning.DB, func(), error) {
	raw, err := badger.Open(badger.DefaultOptions(sstDir).WithValueDir(vlogDir).WithLogger(nil).WithReadOnly(vcsReadOnly && readOnlyOK))
	if err != nil {
		return nil, nil, err
	}
	db, err := versioning.Open(raw, versioning.WithReadOnly(vcsReadOnly))
	if err != nil {
		_ = raw.Close()
		return nil, nil, err
	}
	return db, func() { _ = raw.Close() }, nil
}
func auditDir() string {
	if vcsAuditDir != "" {
		return vcsAuditDir
	}
	return sstDir + "-audit"
}
func gitExporter() versioning.GitExporter {
	return versioning.GitExporter{FilesystemExporter: versioning.FilesystemExporter{Root: auditDir()}, Commit: true}
}
func printAny(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}
func encodeValue(v []byte) any {
	switch vcsValueOutput {
	case "hex":
		return hex.EncodeToString(v)
	case "base64", "json":
		return base64.StdEncoding.EncodeToString(v)
	default:
		return string(v)
	}
}
func printValue(v []byte) error {
	switch vcsValueOutput {
	case "hex":
		fmt.Println(hex.EncodeToString(v))
	case "base64", "json":
		fmt.Println(base64.StdEncoding.EncodeToString(v))
	case "text":
		if strconv.Quote(string(v)) != "\""+string(v)+"\"" {
			fmt.Println(base64.StdEncoding.EncodeToString(v))
			return nil
		}
		fmt.Println(string(v))
	default:
		return fmt.Errorf("unknown --value-output %q", vcsValueOutput)
	}
	return nil
}
