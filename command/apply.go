package command

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/terraform/addrs"
	"github.com/hashicorp/terraform/backend"
	"github.com/hashicorp/terraform/configs/hcl2shim"
	"github.com/hashicorp/terraform/repl"
	"github.com/hashicorp/terraform/states"
	"github.com/hashicorp/terraform/tfdiags"
)

// ApplyCommand is a Command implementation that applies a Terraform
// configuration and actually builds or changes infrastructure.
type ApplyCommand struct {
	Meta

	// If true, then this apply command will become the "destroy"
	// command. It is just like apply but only processes a destroy.
	Destroy bool
}

func (c *ApplyCommand) Run(args []string) int {
	var destroyForce, refresh, autoApprove, jsonOutput bool
	args = c.Meta.process(args)
	cmdName := "apply"
	if c.Destroy {
		cmdName = "destroy"
	}

	cmdFlags := c.Meta.extendedFlagSet(cmdName)
	cmdFlags.BoolVar(&autoApprove, "auto-approve", false, "skip interactive approval of plan before applying")
	if c.Destroy {
		cmdFlags.BoolVar(&destroyForce, "force", false, "deprecated: same as auto-approve")
	}
	cmdFlags.BoolVar(&refresh, "refresh", true, "refresh")
	cmdFlags.BoolVar(&jsonOutput, "json", false, "output JSON format")
	cmdFlags.IntVar(&c.Meta.parallelism, "parallelism", DefaultParallelism, "parallelism")
	cmdFlags.StringVar(&c.Meta.statePath, "state", "", "path")
	cmdFlags.StringVar(&c.Meta.stateOutPath, "state-out", "", "path")
	cmdFlags.StringVar(&c.Meta.backupPath, "backup", "", "path")
	cmdFlags.BoolVar(&c.Meta.stateLock, "lock", true, "lock state")
	cmdFlags.DurationVar(&c.Meta.stateLockTimeout, "lock-timeout", 0, "lock timeout")
	cmdFlags.Usage = func() { c.Ui.Error(c.Help()) }
	if err := cmdFlags.Parse(args); err != nil {
		return 1
	}

	var diags tfdiags.Diagnostics

	args = cmdFlags.Args()
	configPath, err := ModulePath(args)
	if err != nil {
		diags = diags.Append(err.Error())
		return c.showResults(diags, jsonOutput)
	}

	// Check for user-supplied plugin path
	if c.pluginPath, err = c.loadPluginPath(); err != nil {
		diags = diags.Append(fmt.Sprintf("Error loading plugin path: %s", err))
		return c.showResults(diags, jsonOutput)
	}

	// Check if the path is a plan
	planFile, err := c.PlanFile(configPath)
	if err != nil {
		diags = diags.Append(err.Error())
		return c.showResults(diags, jsonOutput)
	}
	if c.Destroy && planFile != nil {
		diags = diags.Append(fmt.Sprintf("Destroy can't be called with a plan file."))
		return c.showResults(diags, jsonOutput)
	}
	if planFile != nil {
		// Reset the config path for backend loading
		configPath = ""

		if !c.variableArgs.Empty() {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Can't set variables when applying a saved plan",
				"The -var and -var-file options cannot be used when applying a saved plan file, because a saved plan includes the variable values that were set when it was created.",
			))
			// Double check this always returns 1
			c.showResults(diags, jsonOutput)
			return 1
		}
	}

	// Load the backend
	var be backend.Enhanced
	var beDiags tfdiags.Diagnostics
	if planFile == nil {
		backendConfig, configDiags := c.loadBackendConfig(configPath)
		diags = diags.Append(configDiags)
		if configDiags.HasErrors() {
			return c.showResults(diags, jsonOutput)
		}

		be, beDiags = c.Backend(&BackendOpts{
			Config: backendConfig,
		})
	} else {
		plan, err := planFile.ReadPlan()
		if err != nil {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Failed to read plan from plan file",
				fmt.Sprintf("Cannot read the plan from the given plan file: %s.", err),
			))
			// Assert always returns 1
			c.showResults(diags, jsonOutput)
			return 1
		}
		if plan.Backend.Config == nil {
			// Should never happen; always indicates a bug in the creation of the plan file
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Failed to read plan from plan file",
				fmt.Sprintf("The given plan file does not have a valid backend configuration. This is a bug in the Terraform command that generated this plan file."),
			))
			c.showResults(diags, jsonOutput)
			return 1
		}
		be, beDiags = c.BackendForPlan(plan.Backend)
	}
	diags = diags.Append(beDiags)
	if beDiags.HasErrors() {
		return c.showResults(diags, jsonOutput)
	}

	// Before we delegate to the backend, we'll print any warning diagnostics
	// we've accumulated here, since the backend will start fresh with its own
	// diagnostics.
	if jsonOutput {
		tmp := diags[:0]
		for _, d := range diags {
			if d.Severity() == tfdiags.Warning {
				tmp = append(tmp, d)
			}
		}
		diags = tmp
	} else {
		// only show results if we're not outputting JSON
		c.showResults(diags, jsonOutput)
		diags = nil
	}

	// Build the operation
	opReq := c.Operation(be)
	opReq.AutoApprove = autoApprove
	opReq.ConfigDir = configPath
	opReq.Destroy = c.Destroy
	opReq.DestroyForce = destroyForce
	opReq.PlanFile = planFile
	opReq.PlanRefresh = refresh
	opReq.Type = backend.OperationTypeApply

	opReq.ConfigLoader, err = c.initConfigLoader()
	if err != nil {
		diags = diags.Append(err)
		return c.showResults(diags, jsonOutput)
		//return 1
	}

	{
		var moreDiags tfdiags.Diagnostics
		opReq.Variables, moreDiags = c.collectVariableValues()
		diags = diags.Append(moreDiags)
		if moreDiags.HasErrors() {
			return c.showResults(diags, jsonOutput)
		}
	}

	op, err := c.RunOperation(be, opReq)
	if err != nil {
		diags = diags.Append(err.Error())
		return c.showResults(diags, jsonOutput)
	}
	if op.Result != backend.OperationSuccess {
		return op.Result.ExitStatus()
	}

	if !c.Destroy {
		if jsonOutput {
			// c.Ui.Output("This is a test")
		} else {
			if outputs := outputsAsString(op.State, addrs.RootModuleInstance, true); outputs != "" {
				c.Ui.Output(c.Colorize().Color(outputs))
			}
		}
	}

	c.showResults(diags, jsonOutput)
	return op.Result.ExitStatus()
}

func (c *ApplyCommand) Help() string {
	if c.Destroy {
		return c.helpDestroy()
	}

	return c.helpApply()
}

func (c *ApplyCommand) Synopsis() string {
	if c.Destroy {
		return "Destroy Terraform-managed infrastructure"
	}

	return "Builds or changes infrastructure"
}

func (c *ApplyCommand) helpApply() string {
	helpText := `
Usage: terraform apply [options] [DIR-OR-PLAN]

  Builds or changes infrastructure according to Terraform configuration
  files in DIR.

  By default, apply scans the current directory for the configuration
  and applies the changes appropriately. However, a path to another
  configuration or an execution plan can be provided. Execution plans can be
  used to only execute a pre-determined set of actions.

Options:

  -auto-approve          Skip interactive approval of plan before applying.

  -backup=path           Path to backup the existing state file before
                         modifying. Defaults to the "-state-out" path with
                         ".backup" extension. Set to "-" to disable backup.

  -compact-warnings      If Terraform produces any warnings that are not
                         accompanied by errors, show them in a more compact
                         form that includes only the summary messages.

  -lock=true             Lock the state file when locking is supported.

  -lock-timeout=0s       Duration to retry a state lock.

  -input=true            Ask for input for variables if not directly set.

  -no-color              If specified, output won't contain any color.

  -parallelism=n         Limit the number of parallel resource operations.
                         Defaults to 10.

  -refresh=true          Update state prior to checking for differences. This
                         has no effect if a plan file is given to apply.

  -state=path            Path to read and save state (unless state-out
                         is specified). Defaults to "terraform.tfstate".

  -state-out=path        Path to write state to that is different than
                         "-state". This can be used to preserve the old
                         state.

  -target=resource       Resource to target. Operation will be limited to this
                         resource and its dependencies. This flag can be used
                         multiple times.

  -var 'foo=bar'         Set a variable in the Terraform configuration. This
                         flag can be set multiple times.

  -var-file=foo          Set variables in the Terraform configuration from
                         a file. If "terraform.tfvars" or any ".auto.tfvars"
                         files are present, they will be automatically loaded.


`
	return strings.TrimSpace(helpText)
}

func (c *ApplyCommand) helpDestroy() string {
	helpText := `
Usage: terraform destroy [options] [DIR]

  Destroy Terraform-managed infrastructure.

Options:

  -backup=path           Path to backup the existing state file before
                         modifying. Defaults to the "-state-out" path with
                         ".backup" extension. Set to "-" to disable backup.

  -auto-approve          Skip interactive approval before destroying.

  -force                 Deprecated: same as auto-approve.

  -lock=true             Lock the state file when locking is supported.

  -lock-timeout=0s       Duration to retry a state lock.

  -no-color              If specified, output won't contain any color.

  -parallelism=n         Limit the number of concurrent operations.
                         Defaults to 10.

  -refresh=true          Update state prior to checking for differences. This
                         has no effect if a plan file is given to apply.

  -state=path            Path to read and save state (unless state-out
                         is specified). Defaults to "terraform.tfstate".

  -state-out=path        Path to write state to that is different than
                         "-state". This can be used to preserve the old
                         state.

  -target=resource       Resource to target. Operation will be limited to this
                         resource and its dependencies. This flag can be used
                         multiple times.

  -var 'foo=bar'         Set a variable in the Terraform configuration. This
                         flag can be set multiple times.

  -var-file=foo          Set variables in the Terraform configuration from
                         a file. If "terraform.tfvars" or any ".auto.tfvars"
                         files are present, they will be automatically loaded.


`
	return strings.TrimSpace(helpText)
}

func outputsAsString(state *states.State, modPath addrs.ModuleInstance, includeHeader bool) string {
	if state == nil {
		return ""
	}

	ms := state.Module(modPath)
	if ms == nil {
		return ""
	}

	outputs := ms.OutputValues
	outputBuf := new(bytes.Buffer)
	if len(outputs) > 0 {
		if includeHeader {
			outputBuf.WriteString("[reset][bold][green]\nOutputs:\n\n")
		}

		// Output the outputs in alphabetical order
		keyLen := 0
		ks := make([]string, 0, len(outputs))
		for key, _ := range outputs {
			ks = append(ks, key)
			if len(key) > keyLen {
				keyLen = len(key)
			}
		}
		sort.Strings(ks)

		for _, k := range ks {
			v := outputs[k]
			if v.Sensitive {
				outputBuf.WriteString(fmt.Sprintf("%s = <sensitive>\n", k))
				continue
			}

			// Our formatter still wants an old-style raw interface{} value, so
			// for now we'll just shim it.
			// FIXME: Port the formatter to work with cty.Value directly.
			legacyVal := hcl2shim.ConfigValueFromHCL2(v.Value)
			result, err := repl.FormatResult(legacyVal)
			if err != nil {
				// We can't really return errors from here, so we'll just have
				// to stub this out. This shouldn't happen in practice anyway.
				result = "<error during formatting>"
			}

			outputBuf.WriteString(fmt.Sprintf("%s = %s\n", k, result))
		}
	}

	return strings.TrimSpace(outputBuf.String())
}

const outputInterrupt = `Interrupt received.
Please wait for Terraform to exit or data loss may occur.
Gracefully shutting down...`

func (c *ApplyCommand) showResults(diags tfdiags.Diagnostics, jsonOutput bool) int {
	switch {
	case jsonOutput:
		// FIXME: Eventually we'll probably want to factor this out somewhere
		// to support machine-readable outputs for other commands too, but for
		// now it's simplest to do this inline here.
		type Pos struct {
			Line   int `json:"line"`
			Column int `json:"column"`
			Byte   int `json:"byte"`
		}
		type Range struct {
			Filename string `json:"filename"`
			Start    Pos    `json:"start"`
			End      Pos    `json:"end"`
		}
		type Diagnostic struct {
			Severity string `json:"severity,omitempty"`
			Summary  string `json:"summary,omitempty"`
			Detail   string `json:"detail,omitempty"`
			Range    *Range `json:"range,omitempty"`
		}
		type Output struct {
			// We include some summary information that is actually redundant
			// with the detailed diagnostics, but avoids the need for callers
			// to re-implement our logic for deciding these.
			Success      bool         `json:"success"`
			ErrorCount   int          `json:"error_count"`
			WarningCount int          `json:"warning_count"`
			Diagnostics  []Diagnostic `json:"diagnostics"`
		}

		var output Output
		output.Success = true // until proven otherwise
		for _, diag := range diags {
			var jsonDiag Diagnostic
			switch diag.Severity() {
			case tfdiags.Error:
				jsonDiag.Severity = "error"
				output.ErrorCount++
				output.Success = false
			case tfdiags.Warning:
				jsonDiag.Severity = "warning"
				output.WarningCount++
			}

			desc := diag.Description()
			jsonDiag.Summary = desc.Summary
			jsonDiag.Detail = desc.Detail

			ranges := diag.Source()
			if ranges.Subject != nil {
				subj := ranges.Subject
				jsonDiag.Range = &Range{
					Filename: subj.Filename,
					Start: Pos{
						Line:   subj.Start.Line,
						Column: subj.Start.Column,
						Byte:   subj.Start.Byte,
					},
					End: Pos{
						Line:   subj.End.Line,
						Column: subj.End.Column,
						Byte:   subj.End.Byte,
					},
				}
			}

			output.Diagnostics = append(output.Diagnostics, jsonDiag)
		}
		if output.Diagnostics == nil {
			// Make sure this always appears as an array in our output, since
			// this is easier to consume for dynamically-typed languages.
			output.Diagnostics = []Diagnostic{}
		}

		j, err := json.MarshalIndent(&output, "", "  ")
		if err != nil {
			// Should never happen because we fully-control the input here
			panic(err)
		}
		c.Ui.Output(string(j))

	default:
		if len(diags) == 0 {
			c.Ui.Output(c.Colorize().Color("[green][bold]Success![reset] The configuration is valid.\n"))
		} else {
			c.showDiagnostics(diags)

			if !diags.HasErrors() {
				c.Ui.Output(c.Colorize().Color("[green][bold]Success![reset] The configuration is valid, but there were some validation warnings as shown above.\n"))
			}
		}
	}

	if diags.HasErrors() {
		return 1
	}
	return 0
}
