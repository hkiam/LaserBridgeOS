# ADR 0016: A configuration both slots can read

- Status: accepted
- Date: 2026-08-15

## Context

`/data` is shared by both system slots. That is what makes an A/B update
painless - the appliance comes back with its settings - and it is also what
makes the two slots share one file whose schema each of them defines
separately.

The parser rejected any key it did not know, and `Store.Ensure` replaced a
configuration it could not parse with the defaults. Put together, on the one
path where the two versions meet:

1. A new release adds a setting. `usb_autosuspend` was the most recent;
   `monitor_port` and `stationary_beam_seconds` came the same way.
2. The update turns out to be bad, so the appliance boots the previous slot -
   by hand, or through the boot-attempt counter after three failed tries.
3. The older binary reads `/data/config.yaml`, finds `usb_autosuspend`, and
   fails the parse.
4. `Ensure` moves the file aside and writes `Default()`. That has
   `setup_complete: false`, which starts the setup access point, and no Wi-Fi
   credentials, which takes the appliance off the workshop network.

The rollback is the recovery mechanism. In this state it was the thing that
stranded the appliance: a headless box that has left the network, in response
to a configuration file whose only fault was being newer than the code reading
it. Every service is affected, not only the web interface - `ser2net` and
`laserbridged` both ask `laserbridge grbl-backend` whether they should start,
and that reads the same file.

The strictness was not wrong in itself. It was applied at the wrong boundary.

## Decision

**Strict at the API, tolerant at the file.** A JSON request carrying an unknown
field is a caller with a bug and is still refused (`DisallowUnknownFields`). A
file on disk carrying something this version has not heard of was written by a
version that knew more, and is read past:

- an unknown **section** is skipped whole;
- an unknown **key** is skipped;
- a **value** that will not parse leaves its field at the default - which is how
  a range or spelling that a later release widened reaches an older one.

Each is recorded and reported rather than passed over in silence: on stderr,
which is the service log, and in `/api/status`, which the System page shows.
"This version is older than the file" and "the appliance forgot my settings"
look identical from the outside, and only one of them needs acting on.

**The shape stays part of the contract.** Sections of flat `key: value` scalars,
two-space indented. A file that breaks that is still refused, because at that
point "written by a newer version" and "damaged" are indistinguishable, and
tolerating the second would mean an appliance silently running on defaults after
a truncated write. A release that needs a list must add a section, not a shape.

**A file that was understood is not rewritten.** Not even when parts of it were
skipped. Those parts are the newer version's settings, and leaving them in place
is what lets its slot find them where it left them when it is booted again. The
consequence, stated plainly: saving anything from the older version's web
interface does drop them, because `MarshalYAML` writes only what this version
knows. That is the right behaviour - the operator is actively editing on the
older release - and it is the reason the notes name the keys.

**What cannot be used is salvaged, not discarded.** When a configuration still
fails to validate, it is moved to `config.yaml.broken` as before, but the
replacement is no longer `Default()`. `Salvage` keeps what reaching the
appliance depends on - hostname, Wi-Fi, the network section - taking each only
if it validates on its own, and takes defaults for everything else. GRBL and
camera settings are deliberately not salvaged: losing them is annoying and
repairable through the web interface, which is only true for as long as the web
interface can still be found.

`setup_complete` is kept only when there is still a way onto the network: with
the Wi-Fi credentials unsalvageable on a Wi-Fi appliance, the setup access point
is exactly the fallback that case calls for, and suppressing it would leave the
appliance with nothing.

## Consequences

A typo in a hand-edited configuration is now ignored instead of rejected. That
is a real cost and it is why the notes exist: the setting that is being skipped
is named in the log and on the System page, rather than the whole file being
refused. The trade is deliberate - the hand-editing case has an operator
watching, and the rollback case has nobody at all.

Forward compatibility is now a property with a test rather than an intention:
`TestConfigurationFromANewerVersionSurvivesARollback` reads a file containing an
unknown key and an unknown section, and
`TestARolledBackApplianceLeavesTheNewerKeysAlone` asserts the file is still
there afterwards. Both fail loudly if the parser is tightened again.

Unknown keys are skipped, not preserved through a save. Carrying them in the
config struct and re-emitting them would make a rollback fully lossless, and it
would mean every version holding settings it cannot show, validate or explain.
The cheaper half of that guarantee - do not rewrite what you did not fully
understand - covers the case that matters, which is a rollback that only reads.

This does not make the two slots' schemas equal. An older release cannot honour
a setting it has never heard of, so an appliance rolled back to it runs with
that setting at its default. `usb_autosuspend` is the concrete example, and the
answer for it is [ADR 0015](0015-usb-autosuspend-is-off.md): the kernel command
line carries the same default independently, so the rolled-back slot behaves the
way the setting asks even while it cannot read it.
