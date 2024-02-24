#!/bin/bash

RED=$(tput setaf 1)
GREEN=$(tput setaf 2)
YELLOW=$(tput setaf 3)
BLUE=$(tput setaf 4)
BRIGHT=$(tput bold)
NORMAL=$(tput sgr0)

function print-label {
  local label=$1
  local item=$2
  printf "${BLUE}%-20s= ${NORMAL}%s\n" "$label" "$item"
}

function print-info {
  local text=$1
  printf "\n${BRIGHT}%s${NORMAL}\n" "$text"
}

function print-green {
  local text=$1
  printf "${GREEN}%s${NORMAL}\n" "$text"
}

function print-yellow {
  local text=$1
  printf "${YELLOW}%s${NORMAL}\n" "$text"
}

function print-error {
  local text=$1
  printf "\n${RED}ERROR: %s${NORMAL}\n" "$text" 1>&2
}

function print_separator() {
  echo -e "\n-------------------------\n"
}

# validate-target-branch returns the branch version if it's valid, errirs out
function validate-target-branch {
  local branch=$1
  if [[ "$branch" =~ ^release-(.*[^0-9])([0-9]+)$ ]]; then
    # Branches older than release-4.11 are unsupported
    if [[ ${BASH_REMATCH[2]} -lt 11 ]]; then
      print-error "Branch $branch is unsupported, only release-4.11 and newer are supported"
      exit 1
    else
      echo ${BASH_REMATCH[2]}
    fi
  else
    print-error "Branch $branch is unsupported, only 'release-x.y' branches are supported"
    exit 1
  fi
}
