package main

type Context struct{}

func (c *Context) Set(key string, val any)                {}
func (c *Context) Render(tpl string, data map[string]any) {}

type User struct {
	Name string
}

func handler() {
	c := &Context{}
	c.Set("currentUser", User{Name: "Bob"})
	c.Render("index.html", map[string]any{
		"title": "Home",
	})
}
