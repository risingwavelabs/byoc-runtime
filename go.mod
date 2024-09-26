module github.com/risingwavelabs/byoc-runtime

go 1.23.0

require (
	github.com/hashicorp/go-version v1.7.0
	github.com/hashicorp/hc-install v0.9.0
	github.com/hashicorp/terraform-exec v0.21.0
	github.com/risingwavelabs/eris v1.3.1
)

require (
	github.com/ProtonMail/go-crypto v1.1.0-alpha.2 // indirect
	github.com/apparentlymart/go-textseg/v15 v15.0.0 // indirect
	github.com/cloudflare/circl v1.3.7 // indirect
	github.com/hashicorp/go-cleanhttp v0.5.2 // indirect
	github.com/hashicorp/go-retryablehttp v0.7.7 // indirect
	github.com/hashicorp/terraform-json v0.22.1 // indirect
	github.com/zclconf/go-cty v1.14.4 // indirect
	golang.org/x/crypto v0.25.0 // indirect
	golang.org/x/mod v0.21.0 // indirect
	golang.org/x/net v0.27.0 // indirect
	golang.org/x/sys v0.22.0 // indirect
	golang.org/x/text v0.16.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240708141625-4ad9e859172b // indirect
	google.golang.org/grpc v1.64.1 // indirect
)

replace github.com/hashicorp/terraform-exec v0.21.0 => github.com/risingwavelabs/terraform-exec v0.21.0-gracefulcancel
