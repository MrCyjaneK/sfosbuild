# sfosbuild

> Build sfos apps, without the hassle

## Installation

```bash
go install github.com/mrcyjanek/sfosbuild@latest
```

## Usage

```bash
Usage:
  sfosbuild [options] <sfos-version> <arch[,arch...]|all> <yaml>
  sfosbuild build [options] <sfos-version> <arch[,arch...]|all> <yaml>
  sfosbuild shell [options] <sfos-version> <arch> [command...]
  sfosbuild deploy [options] <user@host> <yaml>
  sfosbuild --in-place <sfos-version> <arch> <yaml>
```

The last argument is one spectacle yaml or rpm spec, not the repository directory. A file inside `rpm/` packages the directory that contains `rpm/`. Any other file packages the directory that contains it.

`--in-place` is the same working-tree rpmbuild deploy already uses. Run `make clean` and `make distclean` in the project first so a previous architecture's build cache is not packaged. Deploy installs the package it built.

## Hooks

If you need to do some preparations inside of the image `.sfosbuild/image/*.sh`
can be used to put hook scripts in there that run at the docker prepare stage.
If anything there changes a rebuild will happen.