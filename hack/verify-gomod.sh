#!/bin/bash
# Copyright 2024 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.


set -o errexit
set -o nounset
set -o pipefail

echo "Verifying gomod..."

export GO111MODULE=on

echo "go mod tidy"
go mod tidy
echo "go mod vendor"
go mod vendor

diff=$(git diff)

if [[ -n "${diff}" ]]; then
  echo "${diff}"
  echo
  echo "error"
  exit 1
fi

echo "No issue found."
