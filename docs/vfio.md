# VFIO passthrough (experimental)

Linux only and experimental. It has passed its tests but has not yet run on a real passthrough, so the
commands and messages can still change.

Passing a card to a virtual machine through VFIO takes it away from the host for the whole
VM session. The NVIDIA driver lets go, `vfio-pci` takes the card, and nothing on the host may touch it
until the VM is done. Berth coordinates its side of that handover. It drains your engines, holds the
card, runs your own switch script, and keeps the next engine waiting until the card is back on its host
driver.

## What Berth does

- Drains every engine on the card at its boundary, as for any other lease.
- Holds the card for the whole session with a pinned lease at hold priority, so nothing else starts on
  it and nothing drains it.
- Stops berthd's own `nvidia-smi` probe for the whole hold, then checks which processes still hold
  the card's `/dev/nvidiaN`, its DRM nodes, or the shared `/dev/nvidiactl` and `/dev/nvidia-uvm`
  without another card's node. Any holder stops the run, and Berth names it by PID and device node.
  An engine that runs on another NVIDIA card keeps the shared nodes open and does not stop the run.
- Runs your script with the terminal attached, so its sudo prompt and the VM's console reach you, and
  with `BERTH_VFIO_DEVICES` set to the card's PCI functions.
- After the script exits, waits until every function of the card is off `vfio-pci` and the card
  answers `nvidia-smi`, and only then releases the hold.

## What Berth leaves to you

- The driver switch. Berth never binds or unbinds a driver and never writes to sysfs.
- Passwords. Your script calls sudo, and the prompt comes to your terminal.
- The VM, libvirt, QEMU and every other device you pass through.
- Processes that hold the card. Berth names them and refuses; it never stops them.

## Requirements

- An IOMMU. Turn on VT-d (Intel) or AMD-Vi in the firmware, and add `intel_iommu=on iommu=pt` (or
  `amd_iommu=on iommu=pt`) to the kernel command line.
- A second GPU or an iGPU for the host's display, because the passed-through card leaves the host.
- A clean IOMMU group. The card's group may hold only the card's own functions, such as its audio
  function. `berth vfio status` lists the group's members and their drivers, and `berth vfio run`
  refuses while anything else is in the group.
- An NVIDIA card. AMD cards are not covered yet.

## The commands

`berth vfio status` reads the card's state and changes nothing. It shows whether IOMMU is on, the IOMMU
group and the driver bound to each function, the processes holding the card's device nodes, and what
berthd reports. While the card is bound to `vfio-pci`, `nvidia-smi` cannot see it, so pass the card's
address yourself (`berth vfio status 0000:01:00.0`).

`berth vfio run [--gpu CARD] [--reason NAME] [--before CMD] [--after CMD] -- CMD` holds the card and
runs your script. On a node with several cards it takes the first one. `--gpu gpu1` (or the card's PCI
address) passes another card through, and only that card's engines drain:

```sh
berth vfio run --reason galaxy \
  --before "systemctl --user stop llama-swap.service" \
  --after "systemctl --user start llama-swap.service" \
  -- ~/bin/gpu-to-galaxy
```

Engines drain first. Once the card is granted, Berth runs `--before`, if you gave one. Use it to stop
your own services that keep the card open without a lease, such as a monitor that runs `nvidia-smi` in
a loop. berthd stops its own probe for the hold. After a few seconds Berth reads the device holders. If
any process still holds the card, Berth names it, releases the hold and exits 1. A process whose open files Berth cannot read (another user's, usually
root's) gets a warning instead, because Berth cannot prove it clear. Such a process can block the unbind.

When your script exits, Berth releases the hold only if the card is back. Otherwise it keeps the hold,
names the function still on `vfio-pci`, and waits. Each Ctrl-C checks again and releases only once the
card is back. `berth vfio run` exits with your script's exit code.

`--after` runs once the hold has ended, whenever `--before` ran. That covers the card coming back, a
refusal and a failed `--before`. What `--before` stopped always comes back, and never while the card
is still in the VM. Both run with `sh -c`, the terminal attached and `BERTH_VFIO_DEVICES` set.

While the hold is granted, berthd stops opening the card and `berth status` shows it as passed through
to a VM. Waiting requests are told why. ComfyUI jobs sent during the session can wait for the card's return
on disk instead of on the connection (`on_busy = "queue"`, see [Gateway](gateway.md)), and a background
engine starts once the card is back and free.

## Two NVIDIA cards

berthd governs every card by default, and `berth vfio run` has no flag to name a card. It holds the first card
its berthd governs. To pass the other card to a VM while the first keeps serving, run one berthd for each card
and name each card by its PCI address, not its index. `nvidia-smi` renumbers the cards that stay on the host when one goes to a VM, so an index can
point at the other card. An address cannot. To find the addresses, run
`nvidia-smi --query-gpu=index,name,pci.bus_id --format=csv` while both cards are on the host.

`berth service install` writes one `berthd` unit, so install it for the card that stays on the host and
run the second daemon yourself, for the card that goes to the VM, with its own socket and state files:

```sh
berth service install --gpu 0000:01:00.0
berth daemon --gpu 0000:02:00.0 \
  --socket "$XDG_RUNTIME_DIR/berth-vm.sock" \
  --counters ~/.local/state/berth/vm-counters.json --usage ~/.local/state/berth/vm-usage.json \
  --footprint ~/.local/state/berth/vm-footprint.json --devices ""
```

Commands for the second card take its socket: `BERTH_SOCKET=$XDG_RUNTIME_DIR/berth-vm.sock berth vfio run
...`. A berthd started with an address also starts while its card is bound to `vfio-pci`, shows it as
passed through, and takes it over when it comes back. With an index, that berthd refuses to start until
the card is back on its host driver.

Engines on the other card can keep running through the switch. Start each one with
`CUDA_VISIBLE_DEVICES` set to its own card's UUID (`nvidia-smi -L` lists them). An engine without it
opens both cards, and `berth vfio run` names it as a holder.

One gap remains. The other card's berthd runs `nvidia-smi` over both cards, and that can open the card
being switched for a moment. If the unbind in your script hangs, stop the other berthd in `--before`
and start it again in `--after`.

## Examples to adapt

[contrib/vfio](https://github.com/T-Crypt/berth/tree/main/contrib/vfio) has a complete launcher for a
libvirt VM (`vm-session.sh`) and the GPU part of the domain (`domain-hostdev.xml`). With managed host
devices, libvirt switches the drivers, and the launcher only starts the VM and waits for it.

Without libvirt's managed devices, your script switches the drivers itself, as below.

This is an example, not something Berth installs. It moves every function the generic way
(`driver_override`), so it works for the card's audio function too, and it reads the addresses from
`BERTH_VFIO_DEVICES`. If your libvirt domain marks the card's `hostdev` as `managed='yes'`, libvirt
switches the drivers itself, and the script only starts the VM and waits.

```sh
#!/bin/sh
# gpu-to-galaxy: give the card to the VM, run it, take the card back.
# Run it through: berth vfio run --reason galaxy -- ~/bin/gpu-to-galaxy
set -eu
devs="${BERTH_VFIO_DEVICES:?run this through berth vfio run}"

to() { # to DRIVER: move every function of the card to DRIVER ("" = its own driver)
  for d in $devs; do
    echo "$1" | sudo tee "/sys/bus/pci/devices/$d/driver_override" >/dev/null
    if [ -e "/sys/bus/pci/devices/$d/driver" ]; then
      echo "$d" | sudo tee "/sys/bus/pci/devices/$d/driver/unbind" >/dev/null
    fi
    echo "$d" | sudo tee /sys/bus/pci/drivers_probe >/dev/null
  done
}

sudo modprobe vfio-pci
to vfio-pci
sudo virsh start galaxy
while [ "$(sudo virsh domstate galaxy)" != "shut off" ]; do sleep 5; done
to ""
```

If the script dies with the card still on `vfio-pci`, the hold stays, and the next engine waits until
the card is back.
