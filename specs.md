# `devc` specs

## Invocation

```sh
devc [options...] [command] [mode]  <command-args...>
```

where `[mode]` is one of `quarantine`, `review`, `work`, `install`; and `[command]` is one of `start`, `stop`, `pause`, `restart`, `sync`, `status`, `shell`, and `run`. `[options...]` are optionally any of `--image|-i [imagename]`, `--projdir|-p [project directory]`, `--cwd|-d [working directory]`, `--one-off|-1`, `--help|-h`.

## Modes

`[mode]` should determine the access rights that the container gets:

- `install`: The `..._cache`-volumes are mounted read-write, and the starting working directory is `/home/dev`. The idea here is that in this mode, one would be able to install required things into the cache volumes, but we don't launch into the `/workspace` as a matter of caution, in case it contains something malicious: the user should need to actively choose to read anything from there.
- `work` should have cache dirs mounted read-only, and with work dir set to `/workspace`.
- `review` everything is mounted read-only, the working dir is mounted to /src and rsync'd to the `$scratch_vol`.
- `quarantine` should be like `review`, except that network is disabled (basically like `quarantine` already does).

The `--name` of the container should be determined by the project name, directory, and mode.

## Commands

`[command]` should indicate what is being done:

- `status` should basically show info about running containers. When command is `status`, then `[mode]` should be optional: if it is given, only show the status for that container, otherwise for all containers for the project dir.
- `start` should ensure that the container is running in the background, basically with `sleep infinity`. If it is already started, `quarantine` / `review` should not re-rsync.
- `stop` should stop and delete the container.
- `shell` should start the container and start interactive shell in the container
- `run` should run a given shell command that is given in `<command args...>`.
- `pause` should stop the container without deleting it
- `restart` should be equivalent to `stop` and then `start`, i.e. stop the container, delete it, then start it again.
- `sync`: synchronize with `rsync` in `quarantine` / `review` mode.

for `shell` and `run` only, there is the option `--one-off`: it should basically do podman run `--rm` instead of `-d` (and use a different name so that it does not conflict with running containers).

## Options

The other options:

- `--cwd` should be evaluated first: if it is given, we switch the current working directory to there
- `--projdir` should then be evaluated: if it is not given, use the git rev-parse mechanism that we already have and fall back to the (possibly changed by `--cwd`) PWD. If projdir is given as a relative path, it is evaluated relative to before the PWD was changed by `--cwd`. The final PWD must *always* the project dir, or a subdirectory, i.e. reachable from the project directory without leaving the project directory.
- `--image` works as currently already: if it is not given, the present heuristic is used to choose between images, I will extend this later.
- `--help` should give some informative help message. If `--help` is given, everything else should be ignored, and it should be possible to do `--help` without any other arguments.
- if no args are given, or if the given arguments are wrong or contradictory, a short message should be printed, indicating how the command should be executed.

## Possible Extensions

- `--verbose|-v` argument that logs important info (e.g. which image is used, which paths are used etc). Maybe with extra levels (multiple `-v`) where more info is logged, or where `podman` is also invoked in verbose mode
- `--sudo` mode for `run` / `shell` for installing system dependencies.