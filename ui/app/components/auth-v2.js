import { action } from '@ember/object';
import { service } from '@ember/service';
import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';

export default class AuthV2Component extends Component {
  @service permissions;

  @tracked namespace;
  @tracked authType;
  @tracked mountPath;

  constructor() {
    super(...arguments);
    this.namespace = this.args.namespace || '';
    this.mountPath = this.args.mountPath || '';
    if (this.args.wrappedToken) {
      // Only the token component handles wrapped tokens
      this.authType = 'token';
    } else {
      this.authType = this.args.authType || 'token';
    }
  }

  get authMethods() {
    return ['token', 'userpass', 'oidc'];
    // return ['token', 'userpass', 'ldap', 'okta', 'jwt', 'oidc', 'radius', 'github'];
  }

  @action
  handleChange(evt) {
    // For changing values in this backing class, not on form
    const { name, value } = evt.target;
    this[name] = value;
    if (name === 'authType') {
      // if the authType changes, reset the mount path
      this.mountPath = '';
    }
    if (this.args.onUpdate) {
      // Do parent side effects like update query params
      this.args.onUpdate(name, value);
    }
  }

  @action onSuccess() {
    this.permissions.getPaths.perform();
    if (this.args.onSuccess) {
      // Do parent side effects like show flash message for root token
      this.args.onSuccess();
    }
  }
}
