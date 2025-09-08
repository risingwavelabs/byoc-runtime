// Package module defines functions working with Terraform modules.
package module

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/risingwavelabs/eris"
	"github.com/zclconf/go-cty/cty"
)

// InjectCustomModuleRegistry injects private registry endpoint to the source
// attribute for all modules using a public module from Hashicorp default
// registry, so that modules will be downloaded from the specified private
// registry instead.
func InjectCustomModuleRegistry(directory string, privateEndpoint string) error {
	if _, err := os.Stat(directory); os.IsNotExist(err) {
		return eris.Errorf("error: directory '%s' not found", directory)
	}

	walkErr := filepath.Walk(directory, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err // Propagate errors.
		}

		if info.IsDir() || !strings.HasSuffix(info.Name(), ".tf") {
			return nil
		}

		contentBytes, err := os.ReadFile(path)
		if err != nil {
			return eris.Wrapf(err, "failed to read file %s", path)
		}

		hclFile, diags := hclwrite.ParseConfig(contentBytes, path, hcl.InitialPos)
		if diags.HasErrors() {
			return eris.Errorf("failed to parse file %s, error: %s", path, diags.Error())
		}

		body := hclFile.Body()
		var modified bool

		for _, block := range body.Blocks() {
			if block.Type() == "module" {
				blockModified, err := processModuleBlock(privateEndpoint, block)
				if err != nil {
					return err
				}
				modified = blockModified || modified
			}
		}

		if modified {
			// Write the modified content back to the file, formatted nicely.
			err := os.WriteFile(path, hclwrite.Format(hclFile.Bytes()), info.Mode())
			if err != nil {
				return eris.Wrapf(err, "error overwriting file %s", path)
			}
		}
		return nil
	})

	return walkErr
}

func processModuleBlock(privateEndpoint string, block *hclwrite.Block) (bool, error) {
	var moduleName string

	if len(block.Labels()) == 0 {
		return false, eris.Errorf("missing module name in block, %v", block)
	}
	moduleName = block.Labels()[0]

	sourceAttr := block.Body().GetAttribute("source")
	if sourceAttr == nil {
		return false, eris.Errorf("cannot find source attribute in module %s", moduleName)
	}

	exprTokens := sourceAttr.Expr().BuildTokens(nil)
	if len(exprTokens) < 3 {
		return false, eris.Errorf("length parsed tokens of source attribute in module %s is less than 3, %v", moduleName, exprTokens)
	}

	// Tokens is in the order of left quote, name, right quote.
	sourcePath := string(exprTokens[1].Bytes)

	// Check for and skip local paths and paths already using custom registry.
	if strings.HasPrefix(sourcePath, "./") || strings.HasPrefix(sourcePath, "../") || strings.HasPrefix(sourcePath, fmt.Sprintf("%s/", privateEndpoint)) {
		return false, nil
	}

	newSourcePath := fmt.Sprintf("%s/%s", privateEndpoint, sourcePath)

	block.Body().SetAttributeValue("source", cty.StringVal(newSourcePath))
	return true, nil
}
