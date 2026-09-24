# Homebrew release update

The public [argon-lab/homebrew-tap](https://github.com/argon-lab/homebrew-tap)
formula builds `cli/` from the reviewed source archive. It requires Go at build
time and exposes both `argon` and `argonctl`. There are currently no bottles.

After the engine's tag validation/release succeeds, update `argonctl.rb` in the
tap repository using the **same archive URL as the formula**:

```sh
curl --fail --location https://github.com/argon-lab/argon/archive/refs/tags/v2.1.2.tar.gz | shasum -a 256
```

Set its `url` and `sha256`; the linker version symbol for v2 is
`github.com/argon-lab/argon/v2/pkg/version.Build`. Keep this synchronized with
the source module path. Review the formula diff and test in a disposable tap or
clean runner before publishing the tap change:

```sh
brew install --build-from-source argon-lab/tap/argonctl
brew test argon-lab/tap/argonctl
argon --version
argonctl --version
```

Both binaries must report the exact new version. A matching archive checksum
alone does not replace the build and formula tests. Commit and push the reviewed
tap update only after those checks pass. Package publication does not deploy the
hosted demo.
