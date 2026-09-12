package egress

import (
	"fmt"
	"maps"
)

// SaveTokens captures issued placeholders for a network checkpoint.
// Values and injection permissions remain in the current policy.
func (p *Policy) SaveTokens() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	tokens := maps.Clone(p.tokens)
	for _, inj := range p.Injections {
		if inj.Name != "" && inj.Token != "" {
			if tokens == nil {
				tokens = make(map[string]string)
			}
			tokens[inj.Name] = inj.Token
		}
	}
	return tokens
}

// RestoreTokens restores issued placeholders before a resumed guest attaches.
// It validates the complete checkpoint before changing any token. With replace
// false, existing issued tokens must match; previously unissued tokens may join
// an active network. Replacement is for a fresh policy built during resume.
func (p *Policy) RestoreTokens(tokens map[string]string, replace bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for name, token := range tokens {
		if token == "" {
			return fmt.Errorf("egress: saved token %q is empty", name)
		}
		if current, issued := p.tokens[name]; !replace && issued && current != token {
			return fmt.Errorf("egress: saved token %q differs from an already issued token", name)
		}
		found := false
		for _, inj := range p.Injections {
			if inj.Name == name && name != "" {
				found = true
				if inj.Token != "" && inj.Token != token {
					return fmt.Errorf("egress: saved token %q differs from its configured token", name)
				}
				break
			}
		}
		if !found {
			return fmt.Errorf("egress: saved token %q has no injection in the current policy", name)
		}
	}
	if len(tokens) != 0 && p.tokens == nil {
		p.tokens = make(map[string]string)
	}
	maps.Copy(p.tokens, tokens)
	return nil
}
