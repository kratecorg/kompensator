# Contributing to kompensator

Thanks for contributing to kompensator.

## Ways to contribute

- report bugs or rough edges
- improve documentation and examples
- add tests for configuration and reconciliation logic
- fix issues in the CLI, controller, or node agent flows
- propose or implement small features with a focused scope

## Development setup

This project is built with Go.

```bash
git clone https://github.com/kratecorg/kompensator.git
cd kompensator
make build
make test
```

Useful commands:

```bash
make build   # build the CLI binary
make test    # run the test suite
make vet     # run go vet
make race    # run tests with the race detector
```

## Local workflow

1. Create a topic branch for your change.
2. Keep the scope narrow and make the change easy to review.
3. Add or update tests where behavior changes.
4. Run the relevant checks before opening a pull request.
5. Keep documentation in sync when you change commands, config, or workflows.

## Pull request guidelines

- target the default branch
- explain the problem and the solution clearly
- include a short summary of validation performed
- call out any migration or operational implications
- keep the diff focused and reviewable

## Code quality

- prefer clear, readable Go code over cleverness
- keep functions and data structures focused on a single responsibility
- avoid introducing unnecessary dependencies
- keep configuration, CLI behavior, and documentation aligned

## Reporting issues

Use the GitHub issue tracker for bug reports and feature requests.

For suspected security vulnerabilities, use the private security reporting path rather than a public issue.
