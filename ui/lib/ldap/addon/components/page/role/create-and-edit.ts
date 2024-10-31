/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: BUSL-1.1
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { service } from '@ember/service';
import { task } from 'ember-concurrency';
import { waitFor } from '@ember/test-waiters';
import errorMessage from 'vault/utils/error-message';

import type LdapRoleModel from 'vault/models/ldap/role';
import { Breadcrumb, ValidationMap } from 'vault/vault/app-types';
import type FlashMessageService from 'vault/services/flash-messages';
import type PaginationService from 'vault/services/pagination';
import type RouterService from '@ember/routing/router-service';
import type StoreService from 'vault/services/store';
import type { HTMLElementEvent } from 'vault/forms';

interface Args {
  model: LdapRoleModel;
  breadcrumbs: Array<Breadcrumb>;
}
interface RoleTypeOption {
  title: string;
  icon: string;
  description: string;
  value: string;
}

export default class LdapCreateAndEditRolePageComponent extends Component<Args> {
  @service declare readonly flashMessages: FlashMessageService;
  @service('app-router') declare readonly router: RouterService;
  @service declare readonly pagination: PaginationService;
  @service declare readonly store: StoreService;

  @tracked modelValidations: ValidationMap | null = null;
  @tracked invalidFormMessage = '';
  @tracked error = '';

  @tracked newRecord: LdapRoleModel | null = null;

  get model() {
    return this.newRecord || this.args.model;
  }

  get roleTypeOptions(): Array<RoleTypeOption> {
    return [
      {
        title: 'Static role',
        icon: 'user',
        description: 'Static roles map to existing users in an LDAP system.',
        value: 'static',
      },
      {
        title: 'Dynamic role',
        icon: 'folder-users',
        description: 'Dynamic roles allow Vault to create and delete a user in an LDAP system.',
        value: 'dynamic',
      },
    ];
  }

  @action
  handleTypeChange(event: HTMLElementEvent<HTMLInputElement>) {
    const type = event.target.name;
    const backend = this.args.model.backend;
    this.newRecord = this.store.createRecord(`ldap/role/${type}`, { backend });
    console.log(this.store.peekAll('ldap/role'));
    console.log('static', this.store.peekAll('ldap/role/static'));
    console.log('dynamic', this.store.peekAll('ldap/role/dynamic'));
  }

  @task
  @waitFor
  *save(event: Event) {
    event.preventDefault();

    const { model } = this.args;
    const { isValid, state, invalidFormMessage } = model.validate();

    this.modelValidations = isValid ? null : state;
    this.invalidFormMessage = isValid ? '' : invalidFormMessage;

    if (isValid) {
      try {
        const action = model.isNew ? 'created' : 'updated';
        yield model.save();
        this.flashMessages.success(`Successfully ${action} the role ${model.name}`);
        if (action === 'created') {
          this.pagination.clearDataset('ldap/role');
        }
        this.router.transitionTo(
          'vault.cluster.secrets.backend.ldap.roles.role.details',
          model.type,
          model.name
        );
      } catch (error) {
        this.error = errorMessage(error, 'Error saving role. Please try again or contact support.');
      }
    }
  }

  @action
  cancel() {
    this.args.model.rollbackAttributes();
    this.router.transitionTo('vault.cluster.secrets.backend.ldap.roles');
  }
}
