# Testing a pre-release

Work for the next release lands on the `next` branch before it reaches `main`. When it is ready for testers, it is
tagged as a pre-release (`v0.4.1-rc.1`, then `rc.2` and so on) on the
[releases page](https://github.com/T-Crypt/berth/releases), marked **Pre-release**.

Your normal install is not affected. The README's download link, `go install ...@latest` and `berth update` all stay
on the latest release, and `berth update` never offers a pre-release.

A pre-release can change before the release. A new config key may be renamed, and an experimental feature may change
how it works. Back up `berth.toml` before you switch.

## Install a pre-release

Pick the tag from the releases page, then on Linux:

```sh
TAG=v0.4.1-rc.1
cd /tmp
curl -fLO https://github.com/T-Crypt/berth/releases/download/$TAG/berth-linux-amd64
curl -fLO https://github.com/T-Crypt/berth/releases/download/$TAG/SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS
install -Dm755 berth-linux-amd64 ~/.local/bin/berth
berth version
systemctl --user restart berthd berth-harbor
```

Restart `berth-gateway` too if you run it. On Windows, download `berth-windows-amd64.exe` from the same release, put
it where your current `berth.exe` is, and restart Berth the way you started it.

Or build it with Go 1.27 or newer:

```sh
go install github.com/T-Crypt/berth/cmd/berth@v0.4.1-rc.1   # a tagged pre-release
go install github.com/T-Crypt/berth/cmd/berth@next          # the newest commit on next
```

## Go back to the release

Install the latest release the same way, from
[releases/latest](https://github.com/T-Crypt/berth/releases/latest), restore your `berth.toml` backup if you
changed it, and restart the services.

## Report what you find

Comment on the issue the pre-release names for your feature, with `berth version`, your OS and card, and
`berth status`. For a problem that isn't tied to an issue, open a new one and say which pre-release you ran.
