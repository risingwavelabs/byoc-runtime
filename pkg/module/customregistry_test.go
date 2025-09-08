package module

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInjectCustomModuleRegistry(t *testing.T) {
	const privateEndpoint = "private.registry.test/my-org"

	tempDir := t.TempDir()

	testFiles := []struct {
		path            string
		initialContent  string
		expectedContent string
	}{
		{
			// This file tests the main success and ignore cases.
			path: "main.tf",
			initialContent: `
module "remote_module_1" {
  source = "hashicorp/consul/aws"
  version = "0.10.0"
}

# This local module should be ignored.
module "local_module_parent" {
  source = "../local_module"
}

# This local module should also be ignored.
module "local_module_sibling" {
  source = "./another_local_module"
}

# This module should also be ignored as it's already using custom registry.
module "module_sibling_already_using_custom_registry" {
  source = "private.registry.test/my-org/another_local_module"
}

# This remote module with a double slash for subdirectories should be modified.
module "remote_module_2" {
  source = "terraform-aws-modules/vpc/aws//examples/simple-vpc"
}

# This block should be completely ignored by the logic.
terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 4.0"
    }
  }
}
`,
			expectedContent: `
module "remote_module_1" {
  source  = "private.registry.test/my-org/hashicorp/consul/aws"
  version = "0.10.0"
}

# This local module should be ignored.
module "local_module_parent" {
  source = "../local_module"
}

# This local module should also be ignored.
module "local_module_sibling" {
  source = "./another_local_module"
}

# This module should also be ignored as it's already using custom registry.
module "module_sibling_already_using_custom_registry" {
  source = "private.registry.test/my-org/another_local_module"
}

# This remote module with a double slash for subdirectories should be modified.
module "remote_module_2" {
  source = "private.registry.test/my-org/terraform-aws-modules/vpc/aws//examples/simple-vpc"
}

# This block should be completely ignored by the logic.
terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 4.0"
    }
  }
}
`,
		},
		{
			// This file tests the recursive directory walk.
			path: filepath.Join("modules", "nested.tf"),
			initialContent: `
module "another_remote" {
  source = "GoogleCloudPlatform/lb-http/google"
}
`,
			expectedContent: `
module "another_remote" {
  source = "private.registry.test/my-org/GoogleCloudPlatform/lb-http/google"
}
`,
		},
		{
			// This file contains no modules and should be unchanged.
			path: "resources.tf",
			initialContent: `
resource "null_resource" "example" {}
`,
			expectedContent: `
resource "null_resource" "example" {}
`,
		},
	}

	// Create the test files and directories.
	for _, file := range testFiles {
		fullPath := filepath.Join(tempDir, file.path)
		err := os.MkdirAll(filepath.Dir(fullPath), 0755)
		require.NoErrorf(t, err, "Failed to create directories for %s", file.path)
		err = os.WriteFile(fullPath, []byte(file.initialContent), 0644)
		require.NoErrorf(t, err, "Failed to write initial content to %s", file.path)
	}

	err := InjectCustomModuleRegistry(tempDir, privateEndpoint)
	require.NoError(t, err)

	// Verify the contents of each file after running the function.
	for _, file := range testFiles {
		fullPath := filepath.Join(tempDir, file.path)
		actualContentBytes, err := os.ReadFile(fullPath)
		require.NoErrorf(t, err, "Failed to read file for verification %s", file.path)

		actualContent := strings.TrimSpace(string(actualContentBytes))
		expectedContent := strings.TrimSpace(file.expectedContent)

		assert.Equal(t, expectedContent, actualContent)
	}
}
