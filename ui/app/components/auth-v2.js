import { action } from '@ember/object';
import { service } from '@ember/service';
import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import errorMessage from 'vault/utils/error-message';

export default class AuthV2Component extends Component {
  @service session;
  @tracked token = '';
  @tracked error = '';
  @tracked authType = 'token';

  get authMethods() {
    return ['token', 'userpass', 'ldap', 'okta', 'jwt', 'oidc', 'radius', 'github'];
  }

  @action
  handleChange(evt) {
    this.token = evt.target.value;
  }

  @action
  async handleLogin(evt) {
    evt.preventDefault();
    const authenticator = `authenticator:${this.authType}`;
    try {
      await this.session.authenticate(authenticator, this.token, { backend: this.type, namespace: '' });
    } catch (e) {
      this.error = errorMessage(e);
    }

    if (this.session.isAuthenticated) {
      // TODO: Show root warning flash message
    }
  }
}