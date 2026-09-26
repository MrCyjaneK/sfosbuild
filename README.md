# sfosbuild

> Build sfos apps, without the hassle

## Installation

```bash
go install github.com/mrcyjanek/sfosbuild@latest
```

## Usage

```bash
Usage:
  sfosbuild [options] <sfos-version> <arch[,arch...]|all> <project>
  sfosbuild build [options] <sfos-version> <arch[,arch...]|all> <project>
  sfosbuild shell [options] <sfos-version> <arch> [command...]
  sfosbuild deploy [options] <user@host> <project>
  sfosbuild --in-place <sfos-version> <arch> <project>
```

`--in-place` is the same working-tree rpmbuild deploy already uses. Run `make clean` and `make distclean` in the project first so a previous architecture's build cache is not packaged.

## Hooks

If you need to do some preparations inside of the image `.sfosbuild/image/*.sh`
can be used to put hook scripts in there that run at the docker prepare stage.
If anything there changes a rebuild will happen.