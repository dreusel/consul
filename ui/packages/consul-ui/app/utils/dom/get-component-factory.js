/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

export default function (owner, key = '-view-registry:main') {
  const components = owner.lookup(key);
  return function (el) {
    const id = el.getAttribute('id');
    if (id) {
      return components[id];
    }
  };
}
