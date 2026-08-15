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
```

## Hooks

If you need to do some preparations inside of the image `.sfosbuild/image/*.sh`
can be used to put hook scripts in there that run at the docker prepare stage.
If anything there changes a rebuild will happen.