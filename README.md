# provider-ssh

The `provider-ssh` is a Crossplane provider designed for executing scripts on a remote machine over SSH.

The SSH provider adds support for `Script` resources with a `managementPolicy` of `Observe`. This means `Script` resources do not manage any external resources, instead they are used only to execute scripts on and fetch data from an external source.

The `provider-ssh` requires:

- A `ProviderConfig` type that references a credentials `Secret`, which contains the remote machine's `IP`, `Port`, and `Username`  and `Private Key`.
- A `Script` resource type that includes the script to be executed and any variables 
with their corresponding values. These variables will be replaced with actual values 
before the script is sent to the remote machine.
- A managed resource controller that reconciles `Script` objects, by connecting 
to the target machine, executing the scripts, and writing the output (`stdout` and `stderr`) 
back to the respective status fields of the object.

> Feel free to submit any issues you encounter.